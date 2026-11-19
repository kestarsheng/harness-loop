package policy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// 一次性审批令牌（T-P2-39）。
//
// 批准此前只是一句「工单状态变成 approved」：策略层放行了，可是真正执行工具的
// mcp-service 完全不知道这次调用被批准过，只能拿 exec 白名单硬拒。结果就是
// 「人点了批准，rm 还是没跑」。
//
// 现在批准会签发一枚短时令牌，把「哪一次调用」钉死在令牌里：
//
//	task_id + tool + args_digest + 过期时间 + 一次性 nonce + HMAC 签名
//
// 签发方只有 harnessloop（持有密钥），执行方 mcp-service 只持有验签密钥；
// agent-service 不持有任何密钥，只把绑定信息原样透传。

const (
	CodeApprovalTokenMissing    = "E_APPROVAL_TOKEN_MISSING"
	CodeApprovalTokenInvalid    = "E_APPROVAL_TOKEN_INVALID"
	CodeApprovalTokenExpired    = "E_APPROVAL_TOKEN_EXPIRED"
	CodeApprovalTokenBinding    = "E_APPROVAL_TOKEN_BINDING"
	CodeApprovalTokenReplay     = "E_APPROVAL_TOKEN_REPLAY"
	CodeApprovalTokenKeyMissing = "E_APPROVAL_TOKEN_KEY_MISSING"
)

const (
	// ApprovalTokenSecretEnv 是 HMAC 密钥。签发方（harnessloop）与执行方
	// （mcp-service）读同一个环境变量；缺省时两端都必须失败关闭。
	ApprovalTokenSecretEnv = "HARNESS_APPROVAL_TOKEN_SECRET"
	// ApprovalTokenTTLEnv 覆盖令牌默认有效期（秒）。
	ApprovalTokenTTLEnv = "HARNESS_APPROVAL_TOKEN_TTL_SECONDS"

	DefaultApprovalTokenTTL = 300 * time.Second

	approvalTokenVersion = 1
)

// TokenError 是验签/绑定失败的统一错误。Code 直接进审计与响应，调用方不必再
// 翻译一次「为什么拒的」。
type TokenError struct {
	Code   string
	Reason string
}

func (e *TokenError) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Reason
}

func newTokenError(code, format string, args ...any) *TokenError {
	return &TokenError{Code: code, Reason: fmt.Sprintf(format, args...)}
}

// TokenErrorCode 返回 err 对应的错误码；不是令牌错误时返回空串。
func TokenErrorCode(err error) string {
	var te *TokenError
	if errors.As(err, &te) {
		return te.Code
	}
	return ""
}

// ApprovalTokenClaims 是令牌载荷。字段刻意短小：令牌要经 tool_config 透传。
type ApprovalTokenClaims struct {
	Version    int    `json:"v"`
	TenantID   string `json:"tid,omitempty"`
	ProjectID  string `json:"pid,omitempty"`
	TaskID     string `json:"task_id"`
	Tool       string `json:"tool"`
	Stage      string `json:"stage,omitempty"`
	ArgsDigest string `json:"args_digest"`
	ApprovalID string `json:"approval_id"`
	Nonce      string `json:"nonce"`
	IssuedAt   int64  `json:"iat"`
	ExpiresAt  int64  `json:"exp"`
}

// ApprovalTokenExpectation 是执行方对令牌的期望绑定。空字段表示「不约束」，
// 但 TaskID/Tool/ArgsDigest 三者缺一不可——少一个就退化成「通用通行证」。
type ApprovalTokenExpectation struct {
	TenantID   string
	ProjectID  string
	TaskID     string
	Tool       string
	ArgsDigest string
	ApprovalID string
}

// ApprovalTokenSink 把签发好的令牌送到执行面。harnessloop 的装配层实现它
// （gRPC 注册到 mcp-service）。
type ApprovalTokenSink interface {
	RegisterApprovalToken(ctx context.Context, token string) error
}

// ApprovalTokenIssuer 只在 harnessloop 内构造。
type ApprovalTokenIssuer struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

// NewApprovalTokenIssuer 用显式密钥构造签发器。空密钥直接报错：调用方必须
// 失败关闭，不能悄悄退化成「无签名令牌」。
func NewApprovalTokenIssuer(secret string) (*ApprovalTokenIssuer, error) {
	return newApprovalTokenIssuer(secret, DefaultApprovalTokenTTL)
}

func newApprovalTokenIssuer(secret string, ttl time.Duration) (*ApprovalTokenIssuer, error) {
	trimmed := strings.TrimSpace(secret)
	if trimmed == "" {
		return nil, newTokenError(CodeApprovalTokenKeyMissing, "%s is empty", ApprovalTokenSecretEnv)
	}
	if ttl <= 0 {
		ttl = DefaultApprovalTokenTTL
	}
	return &ApprovalTokenIssuer{secret: []byte(trimmed), ttl: ttl, now: time.Now}, nil
}

// ApprovalTokenIssuerFromEnv 读环境变量构造签发器。TTL 非法时回落到默认值。
func ApprovalTokenIssuerFromEnv() (*ApprovalTokenIssuer, error) {
	ttl := DefaultApprovalTokenTTL
	if raw := strings.TrimSpace(os.Getenv(ApprovalTokenTTLEnv)); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			ttl = time.Duration(seconds) * time.Second
		}
	}
	return newApprovalTokenIssuer(os.Getenv(ApprovalTokenSecretEnv), ttl)
}

func (i *ApprovalTokenIssuer) TTL() time.Duration {
	if i == nil {
		return 0
	}
	return i.ttl
}

func (i *ApprovalTokenIssuer) SetClock(now func() time.Time) {
	if i != nil && now != nil {
		i.now = now
	}
}

// Issue 给一次已批准的调用签令牌。ApprovalID 必须非空：工单是「人批准了什么」
// 的唯一凭据，没有它就无法拒绝复用。
func (i *ApprovalTokenIssuer) Issue(claims ApprovalTokenClaims) (string, ApprovalTokenClaims, error) {
	if i == nil {
		return "", claims, newTokenError(CodeApprovalTokenKeyMissing, "approval token issuer is not configured")
	}
	if strings.TrimSpace(claims.TaskID) == "" || strings.TrimSpace(claims.Tool) == "" {
		return "", claims, newTokenError(CodeApprovalTokenInvalid, "task_id and tool are required")
	}
	if strings.TrimSpace(claims.ApprovalID) == "" {
		return "", claims, newTokenError(CodeApprovalTokenInvalid, "approval_id is required")
	}
	now := i.now()
	claims.Version = approvalTokenVersion
	claims.IssuedAt = now.Unix()
	claims.ExpiresAt = now.Add(i.ttl).Unix()
	if claims.Nonce == "" {
		claims.Nonce = approvalNonce(claims)
	}

	payload, err := json.Marshal(claims)
	if err != nil {
		return "", claims, err
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	signature := i.sign(body)
	return body + "." + signature, claims, nil
}

func (i *ApprovalTokenIssuer) sign(body string) string {
	mac := hmac.New(sha256.New, i.secret)
	mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// ApprovalTokenVerifier 只在执行面（mcp-service）构造。
type ApprovalTokenVerifier struct {
	secret []byte
	now    func() time.Time
}

func NewApprovalTokenVerifier(secret string) (*ApprovalTokenVerifier, error) {
	trimmed := strings.TrimSpace(secret)
	if trimmed == "" {
		return nil, newTokenError(CodeApprovalTokenKeyMissing, "%s is empty", ApprovalTokenSecretEnv)
	}
	return &ApprovalTokenVerifier{secret: []byte(trimmed), now: time.Now}, nil
}

func ApprovalTokenVerifierFromEnv() (*ApprovalTokenVerifier, error) {
	return NewApprovalTokenVerifier(os.Getenv(ApprovalTokenSecretEnv))
}

func (v *ApprovalTokenVerifier) SetClock(now func() time.Time) {
	if v != nil && now != nil {
		v.now = now
	}
}

// Verify 校验签名、有效期与绑定关系。任何一项不满足都返回 *TokenError，
// 调用方按 Code 拒绝。
func (v *ApprovalTokenVerifier) Verify(token string, expected ApprovalTokenExpectation) (ApprovalTokenClaims, error) {
	if strings.TrimSpace(expected.TaskID) == "" || strings.TrimSpace(expected.Tool) == "" || strings.TrimSpace(expected.ArgsDigest) == "" {
		return ApprovalTokenClaims{}, newTokenError(CodeApprovalTokenBinding, "expectation must bind task_id, tool and args_digest")
	}
	claims, err := v.Parse(token)
	if err != nil {
		return claims, err
	}
	if claims.TaskID != expected.TaskID || claims.Tool != expected.Tool || claims.ArgsDigest != expected.ArgsDigest {
		return claims, newTokenError(CodeApprovalTokenBinding,
			"token is bound to task=%q tool=%q digest=%q", claims.TaskID, claims.Tool, claims.ArgsDigest)
	}
	if expected.ApprovalID != "" && claims.ApprovalID != expected.ApprovalID {
		return claims, newTokenError(CodeApprovalTokenBinding, "token is bound to approval %q", claims.ApprovalID)
	}
	if expected.TenantID != "" && claims.TenantID != "" && claims.TenantID != expected.TenantID {
		return claims, newTokenError(CodeApprovalTokenBinding, "token is bound to tenant %q", claims.TenantID)
	}
	if expected.ProjectID != "" && claims.ProjectID != "" && claims.ProjectID != expected.ProjectID {
		return claims, newTokenError(CodeApprovalTokenBinding, "token is bound to project %q", claims.ProjectID)
	}
	return claims, nil
}

// Parse 只验「令牌本身」：签名、版本、有效期。执行面在注册时用它——那时还
// 不知道要跑什么，绑定比对留到真正执行前（Verify）。
func (v *ApprovalTokenVerifier) Parse(token string) (ApprovalTokenClaims, error) {
	var claims ApprovalTokenClaims
	if v == nil {
		return claims, newTokenError(CodeApprovalTokenKeyMissing, "approval token verifier is not configured")
	}
	body, signature, ok := strings.Cut(strings.TrimSpace(token), ".")
	if !ok || body == "" || signature == "" {
		return claims, newTokenError(CodeApprovalTokenInvalid, "malformed token")
	}
	mac := hmac.New(sha256.New, v.secret)
	mac.Write([]byte(body))
	if subtle.ConstantTimeCompare([]byte(signature), []byte(base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))) != 1 {
		return claims, newTokenError(CodeApprovalTokenInvalid, "signature mismatch")
	}

	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return claims, newTokenError(CodeApprovalTokenInvalid, "payload is not valid base64")
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return claims, newTokenError(CodeApprovalTokenInvalid, "payload is not valid JSON")
	}
	if claims.Version != approvalTokenVersion {
		return claims, newTokenError(CodeApprovalTokenInvalid, "unsupported token version %d", claims.Version)
	}
	if claims.ExpiresAt <= 0 || !v.now().Before(time.Unix(claims.ExpiresAt, 0)) {
		return claims, newTokenError(CodeApprovalTokenExpired, "token expired at %d", claims.ExpiresAt)
	}
	if claims.TaskID == "" || claims.Tool == "" || claims.ArgsDigest == "" || claims.ApprovalID == "" || claims.Nonce == "" {
		return claims, newTokenError(CodeApprovalTokenInvalid, "token is missing required claims")
	}
	return claims, nil
}

// approvalNonce 由工单与调用绑定确定性派生：同一工单重复投递得到同一个
// nonce（重试幂等），不同工单/不同调用则必然不同（无法互相顶替）。
func approvalNonce(claims ApprovalTokenClaims) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		claims.ApprovalID, claims.TenantID, claims.ProjectID,
		claims.TaskID, claims.Tool, claims.ArgsDigest,
	}, "\x00")))
	return hex.EncodeToString(sum[:16])
}

// RegisterApprovalTokenWithRetry 把令牌投递到执行面。执行面可能比审批晚起
// 或正在重连，短暂重试；仍然失败就把错误交给调用方（调用会被拒，不会放行）。
func RegisterApprovalTokenWithRetry(ctx context.Context, sink ApprovalTokenSink, token string) error {
	if sink == nil {
		return newTokenError(CodeApprovalTokenKeyMissing, "approval token sink is not configured")
	}
	const attempts = 3
	var err error
	for i := 0; i < attempts; i++ {
		if err = sink.RegisterApprovalToken(ctx, token); err == nil {
			return nil
		}
		if i == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return err
}

// ApprovalRef 是公开的、不含密钥的绑定指纹，用于审计关联与跨服务透传。
func ApprovalRef(taskID, tool, argsDigest string) string {
	sum := sha256.Sum256([]byte(taskID + "\x00" + tool + "\x00" + argsDigest))
	return "apr_" + hex.EncodeToString(sum[:12])
}

// CanonicallyArgsFromJSON 把一次工具调用的原始参数规范化成 ToolArgs。
//
// 策略层（harnessloop）、agent-service、执行面（mcp-service）三处都必须用
// 同一个规范化结果算 digest，否则令牌永远绑不上——其中一处把
// "rm -f x" 当整串、另一处拆成 argv，签出来的 digest 就是两个值。
func CanonicalArgsFromJSON(raw []byte) ToolArgs {
	var args map[string]any
	if len(raw) == 0 {
		return ToolArgs{}
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return ToolArgs{}
	}
	return CanonicalArgs(args)
}

// CanonicalArgs 与 CanonicalArgsFromJSON 同义，直接吃已解析的参数表。
func CanonicalArgs(args map[string]any) ToolArgs {
	out := ToolArgs{}
	if args == nil {
		return out
	}
	out.Paths = argumentStrings(args, "paths", "path", "file_path", "files")
	out.Command = commandArguments(args, "command", "cmd", "argv")
	out.NetworkMethod = firstStringValue(args, "network_method", "method")
	out.EstimatedCost = firstFloatValue(args, "estimated_cost", "cost")
	return out
}

func argumentStrings(args map[string]any, keys ...string) []string {
	for _, key := range keys {
		value, ok := args[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case string:
			if strings.TrimSpace(typed) != "" {
				return []string{typed}
			}
		case []any:
			out := make([]string, 0, len(typed))
			for _, item := range typed {
				if s, ok := item.(string); ok && s != "" {
					out = append(out, s)
				}
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	return nil
}

func commandArguments(args map[string]any, keys ...string) []string {
	for _, key := range keys {
		value, ok := args[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case string:
			if fields := strings.Fields(typed); len(fields) > 0 {
				return fields
			}
		case []any:
			out := make([]string, 0, len(typed))
			for _, item := range typed {
				if s, ok := item.(string); ok && s != "" {
					out = append(out, s)
				}
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	return nil
}

func firstStringValue(args map[string]any, keys ...string) string {
	for _, key := range keys {
		if s, ok := args[key].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func firstFloatValue(args map[string]any, keys ...string) float64 {
	for _, key := range keys {
		switch typed := args[key].(type) {
		case float64:
			return typed
		case int:
			return float64(typed)
		case json.Number:
			if f, err := typed.Float64(); err == nil {
				return f
			}
		}
	}
	return 0
}
