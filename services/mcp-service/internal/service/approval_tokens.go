package service

import (
	"context"
	"sync"
	"time"

	"agent-platform/pkg/harnessloop/policy"
)

// ApprovalTokenStore 是执行面的「已批准调用」登记簿。
//
// 批准发生在 harnessloop，执行发生在 mcp-service，两边唯一的硬凭据就是这枚
// 短时令牌：签名证明它来自持有密钥的 harnessloop，绑定字段（task/tool/
// args_digest）证明它说的是「这一次调用」，nonce 保证只用一次。
//
// 失败关闭：没有 verifier（密钥缺失）时 Register / WaitFor / Consume 全部拒绝。
type ApprovalTokenStore struct {
	mu      sync.Mutex
	now     func() time.Time
	verify  *policy.ApprovalTokenVerifier
	entries map[string]approvalTokenEntry // key: approval id
	used    map[string]time.Time          // key: nonce → 消费时间
}

type approvalTokenEntry struct {
	token  string
	claims policy.ApprovalTokenClaims
}

// NewApprovalTokenStore 用环境变量里的密钥构造。密钥缺失时同时返回 store 与
// 错误：store 仍可用（但拒绝一切），调用方负责把错误记进日志。
func NewApprovalTokenStore() (*ApprovalTokenStore, error) {
	verifier, err := policy.ApprovalTokenVerifierFromEnv()
	return newApprovalTokenStoreWithVerifier(verifier, time.Now), err
}

func newApprovalTokenStoreWithVerifier(verifier *policy.ApprovalTokenVerifier, now func() time.Time) *ApprovalTokenStore {
	if now == nil {
		now = time.Now
	}
	return &ApprovalTokenStore{
		now:     now,
		verify:  verifier,
		entries: make(map[string]approvalTokenEntry),
		used:    make(map[string]time.Time),
	}
}

func (s *ApprovalTokenStore) SetClock(now func() time.Time) {
	if s == nil || now == nil {
		return
	}
	s.mu.Lock()
	s.now = now
	s.mu.Unlock()
}

// Register 验签后暂存令牌。同一张工单重复注册（投递重试）是幂等的。
func (s *ApprovalTokenStore) Register(token string) (policy.ApprovalTokenClaims, error) {
	if s == nil || s.verify == nil {
		return policy.ApprovalTokenClaims{}, &policy.TokenError{
			Code:   policy.CodeApprovalTokenKeyMissing,
			Reason: policy.ApprovalTokenSecretEnv + " is not configured on the executor",
		}
	}
	claims, err := s.verify.Parse(token)
	if err != nil {
		return claims, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, used := s.used[claims.Nonce]; used {
		return claims, &policy.TokenError{Code: policy.CodeApprovalTokenReplay, Reason: "token nonce already consumed"}
	}
	if existing, ok := s.entries[claims.ApprovalID]; ok && existing.claims.Nonce == claims.Nonce {
		return claims, nil
	}
	s.entries[claims.ApprovalID] = approvalTokenEntry{token: token, claims: claims}
	s.pruneLocked()
	return claims, nil
}

// WaitFor 等一枚能绑定到这次调用的令牌。批准可能晚于执行（调用端先到），
// 所以这里按 wait budget 轮询；超时返回 MISSING，由调用方拒绝并留痕。
// 注意：这里不消费令牌——消费只发生在真正执行前一刻。
func (s *ApprovalTokenStore) WaitFor(ctx context.Context, expected policy.ApprovalTokenExpectation, timeout time.Duration) (policy.ApprovalTokenClaims, error) {
	if s == nil || s.verify == nil {
		return policy.ApprovalTokenClaims{}, &policy.TokenError{
			Code:   policy.CodeApprovalTokenKeyMissing,
			Reason: policy.ApprovalTokenSecretEnv + " is not configured on the executor",
		}
	}
	if timeout <= 0 {
		timeout = 0
	}
	deadline := s.now().Add(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		claims, ok := s.matchLocked(expected)
		s.mu.Unlock()
		if ok {
			return claims, nil
		}
		if !s.now().Before(deadline) {
			return policy.ApprovalTokenClaims{}, &policy.TokenError{
				Code:   policy.CodeApprovalTokenMissing,
				Reason: "no approval token registered for this call within the wait budget",
			}
		}
		select {
		case <-ctx.Done():
			return policy.ApprovalTokenClaims{}, &policy.TokenError{Code: policy.CodeApprovalTokenMissing, Reason: ctx.Err().Error()}
		case <-ticker.C:
		}
	}
}

// Consume 校验绑定并一次性消费 nonce。并发重复消费只有第一个成功。
func (s *ApprovalTokenStore) Consume(expected policy.ApprovalTokenExpectation) (policy.ApprovalTokenClaims, error) {
	if s == nil || s.verify == nil {
		return policy.ApprovalTokenClaims{}, &policy.TokenError{
			Code:   policy.CodeApprovalTokenKeyMissing,
			Reason: policy.ApprovalTokenSecretEnv + " is not configured on the executor",
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.entries[expected.ApprovalID]
	if !ok {
		// 没给 approval id 时按绑定找一枚唯一的令牌。
		if expected.ApprovalID == "" {
			for id, candidate := range s.entries {
				if claims, err := s.verify.Verify(candidate.token, expected); err == nil {
					entry, ok = candidate, true
					expected.ApprovalID = id
					_ = claims
					break
				}
			}
		}
		if !ok {
			return policy.ApprovalTokenClaims{}, &policy.TokenError{Code: policy.CodeApprovalTokenMissing, Reason: "approval token is not registered"}
		}
	}
	if _, used := s.used[entry.claims.Nonce]; used {
		return policy.ApprovalTokenClaims{}, &policy.TokenError{Code: policy.CodeApprovalTokenReplay, Reason: "approval token was already used"}
	}
	claims, err := s.verify.Verify(entry.token, expected)
	if err != nil {
		return claims, err
	}
	s.used[claims.Nonce] = s.now()
	delete(s.entries, expected.ApprovalID)
	s.pruneLocked()
	return claims, nil
}

// matchLocked 找出能绑定这次调用的令牌（不消费）。
func (s *ApprovalTokenStore) matchLocked(expected policy.ApprovalTokenExpectation) (policy.ApprovalTokenClaims, bool) {
	for id, entry := range s.entries {
		if expected.ApprovalID != "" && id != expected.ApprovalID {
			continue
		}
		if _, used := s.used[entry.claims.Nonce]; used {
			continue
		}
		if claims, err := s.verify.Verify(entry.token, expected); err == nil {
			return claims, true
		}
	}
	return policy.ApprovalTokenClaims{}, false
}

func (s *ApprovalTokenStore) pruneLocked() {
	now := s.now()
	for id, entry := range s.entries {
		if !now.Before(time.Unix(entry.claims.ExpiresAt, 0)) {
			delete(s.entries, id)
		}
	}
	for nonce, at := range s.used {
		if now.Sub(at) > time.Hour {
			delete(s.used, nonce)
		}
	}
}
