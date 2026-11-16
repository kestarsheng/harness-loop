package compaction

// engine_adapter.go —— T-P2-05「压缩引擎适配（二期可选）」的 mock 适配层。
//
// 边界声明（重要）：
//   - 外部 context-compression-engine 的选型（U16）尚未拍板，因此本文件不接入任何
//     真实外部引擎，也不引入 Python/运维依赖；这里只有 MockEngine。
//   - U16 拍板后，真实引擎应实现同一个 EngineAdapter 接口（Compact），开关与降级
//     语义保持不变；届时只需替换 EngineAdapter 实现，调用方与事件 schema 不动。
//   - 与 engine.go 中预留的 ContextEngine（JSONL 片段槽，Compress(Fragments)）不同：
//     本层是「消息级」适配器（阶段消息 + 预算 → 压缩后消息 + engine_fragment 标记）。
//     两者共存，EngineAdapter 不复用、也不改变 ContextEngine 的签名。
//   - 事件 schema 不变：engine_fragment 只作为统计/标记字段（LineEngineFragment
//     在 JSONL 中仍属 phase one 保留项，禁止产出），不新增事件类型。

import (
	"context"
	"errors"
	"fmt"

	"agent-platform/pkg/llm"
)

const (
	// DegradeReasonEngineAdapter 是适配器失败并入 L1 降级时使用的 reason 前缀。
	DegradeReasonEngineAdapter = "engine_adapter"

	EngineReasonApplied       = "applied"
	EngineReasonDisabled      = "disabled"
	EngineReasonFailed        = "failed"
	EngineReasonTimeout       = "timeout"
	EngineReasonInvalidOutput = "invalid_output"
	EngineReasonL1Failed      = "l1_failed"

	// DefaultMockBudget 在调用方未给出正预算时兜底，保证 mock 输出有界。
	DefaultMockBudget = 2048
)

var (
	ErrEngineAdapterInvalidOutput = errors.New("compaction: engine adapter returned invalid output")
	ErrEngineAdapterTimeout       = errors.New("compaction: engine adapter timed out")
	ErrEngineAdapterEmptyInput    = errors.New("compaction: engine adapter requires at least one message")
)

// EngineRequest 是适配器的输入：阶段消息 + token 预算。
type EngineRequest struct {
	Phase        string
	Messages     []ChatMessage
	BudgetTokens int
}

func (r EngineRequest) budget() int {
	if r.BudgetTokens <= 0 {
		return DefaultMockBudget
	}
	return r.BudgetTokens
}

// EngineResult 是适配器的输出：压缩后消息 + engine_fragment 标记。
// Fragments 为 0 表示未产出片段；1 表示本次压缩由引擎产出（mock 恒为 1）。
type EngineResult struct {
	Messages       []ChatMessage
	EngineFragment bool
	Fragments      int
	Engine         string
}

// EngineAdapter 是消息级压缩引擎适配接口。MockEngine 是当前唯一实现（U16 未拍板）。
type EngineAdapter interface {
	Compact(ctx context.Context, req EngineRequest) (EngineResult, error)
}

// ---------------------------------------------------------------------------
// MockEngine
// ---------------------------------------------------------------------------

// MockEngine 是确定性 mock 实现：保留 system 前缀 + 预算内尽可能新的消息，
// 丢弃中间历史；不调用任何外部进程/网络。它只用于验证开关、计标记与降级链路。
type MockEngine struct{}

func NewMockEngine() EngineAdapter { return MockEngine{} }

func (MockEngine) Compact(ctx context.Context, req EngineRequest) (EngineResult, error) {
	if err := ctx.Err(); err != nil {
		return EngineResult{}, err
	}
	if len(req.Messages) == 0 {
		return EngineResult{}, ErrEngineAdapterEmptyInput
	}
	budget := req.budget()

	header := ChatMessage{
		Role:    "system",
		Content: fmt.Sprintf("[engine:%s] phase=%s compacted %d messages under budget=%d tokens", EngineNameMock, req.Phase, len(req.Messages), budget),
	}
	out := []ChatMessage{header}
	remaining := budget - estimateMessages(out)
	if remaining < 0 {
		return EngineResult{}, fmt.Errorf("%w: budget %d is smaller than the engine header", ErrEngineAdapterInvalidOutput, budget)
	}

	// 1) system 前缀优先，最多占剩余预算的一半。
	for _, msg := range req.Messages {
		if msg.Role != "system" {
			continue
		}
		keep := truncateMessage(msg, remaining/2)
		out = append(out, keep)
		remaining -= estimateMessages([]ChatMessage{keep})
		break
	}

	// 2) 从最新往回填充，保证近期上下文优先。
	var tail []ChatMessage
	for i := len(req.Messages) - 1; i >= 0; i-- {
		msg := req.Messages[i]
		if msg.Role == "system" || remaining <= 0 {
			continue
		}
		keep := truncateMessage(msg, remaining)
		cost := estimateMessages([]ChatMessage{keep})
		if cost > remaining {
			continue
		}
		tail = append([]ChatMessage{keep}, tail...)
		remaining -= cost
	}
	if issues := ValidatePairing(tail); len(issues) > 0 {
		tail = dropUnpairedToolMessages(tail)
	}
	out = append(out, tail...)

	if err := validateEngineResult(EngineRequest{Phase: req.Phase, Messages: req.Messages, BudgetTokens: budget}, EngineResult{Messages: out}); err != nil {
		return EngineResult{}, err
	}
	return EngineResult{Messages: out, EngineFragment: true, Fragments: 1, Engine: EngineNameMock}, nil
}

func truncateMessage(msg ChatMessage, maxTokens int) ChatMessage {
	if maxTokens <= 0 {
		msg.Content = ""
		return msg
	}
	maxTokens -= len(msg.ToolCalls) * 8 // tool_call 本身也占 token
	if maxTokens <= 0 {
		msg.Content = ""
		return msg
	}
	msg.Content = fitContent(msg.Content, maxTokens)
	return msg
}

// fitContent 按 llm.EstimateTokens 的口径（CJK 约 1.5 token/字）截断到预算内，
// 避免用「字符数≈token 数」的粗算把 mock 输出顶出预算。
func fitContent(content string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	if llm.EstimateTokens(content) <= maxTokens {
		return content
	}
	const suffix = "…[truncated]"
	limit := maxTokens
	if limit > 16 {
		limit = maxTokens - 8 // 给后缀留位
	}
	runes := []rune(content)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if llm.EstimateTokens(string(runes[:mid])) <= limit {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	if lo == 0 {
		return ""
	}
	candidate := string(runes[:lo]) + suffix
	if llm.EstimateTokens(candidate) > maxTokens {
		return string(runes[:lo])
	}
	return candidate
}

var validEngineRoles = map[string]bool{"system": true, "user": true, "assistant": true, "tool": true}

// validateEngineResult 统一校验适配器输出：非空、角色合法、tool 配对完整、不超预算。
// 任何一条不满足都视为「输出非法」，由 RunEngineAdapter 并入 L1 降级。
func validateEngineResult(req EngineRequest, res EngineResult) error {
	if len(res.Messages) == 0 {
		return fmt.Errorf("%w: empty message list", ErrEngineAdapterInvalidOutput)
	}
	for _, msg := range res.Messages {
		if !validEngineRoles[msg.Role] {
			return fmt.Errorf("%w: unknown role %q", ErrEngineAdapterInvalidOutput, msg.Role)
		}
	}
	if issues := ValidatePairing(res.Messages); len(issues) > 0 {
		return fmt.Errorf("%w: %v", ErrEngineAdapterInvalidOutput, PairingError(issues))
	}
	if after := estimateMessages(res.Messages); after > req.budget() {
		return fmt.Errorf("%w: tokens_after=%d exceeds budget=%d", ErrEngineAdapterInvalidOutput, after, req.budget())
	}
	return nil
}

// ---------------------------------------------------------------------------
// slot：开关 + 超时 + 输出校验
// ---------------------------------------------------------------------------

// EngineAdapterSlot 把开关、超时和输出校验收在一处：开关关闭时既不调用适配器，
// 也不产生任何副作用，从而保证「关闭即零行为差异」。
type EngineAdapterSlot struct {
	cfg    EngineAdapterConfig
	engine EngineAdapter
}

func NewEngineAdapterSlot(cfg EngineAdapterConfig, engine EngineAdapter) EngineAdapterSlot {
	return EngineAdapterSlot{cfg: cfg.normalized(), engine: engine}
}

// NewMockEngineSlot 按给定开关装配 mock 适配器（U16 未拍板前的唯一可用实现）。
func NewMockEngineSlot(cfg EngineAdapterConfig) EngineAdapterSlot {
	return NewEngineAdapterSlot(cfg, NewMockEngine())
}

func (s EngineAdapterSlot) Enabled() bool { return s.cfg.Enabled && s.engine != nil }

func (s EngineAdapterSlot) Config() EngineAdapterConfig { return s.cfg }

// Compact 在超时预算内调用适配器并校验其输出。适配器忽略 ctx 时也不会挂住调用方。
func (s EngineAdapterSlot) Compact(ctx context.Context, req EngineRequest) (EngineResult, error) {
	if !s.cfg.Enabled {
		return EngineResult{}, ErrEngineDisabled
	}
	if s.engine == nil {
		return EngineResult{}, ErrEngineNotConfigured
	}
	cctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	type outcome struct {
		res EngineResult
		err error
	}
	// 缓冲 1，避免适配器忽略 ctx 时把 goroutine 永久阻塞在发送上。
	done := make(chan outcome, 1)
	go func() {
		res, err := s.engine.Compact(cctx, req)
		done <- outcome{res: res, err: err}
	}()

	select {
	case <-cctx.Done():
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return EngineResult{}, ErrEngineAdapterTimeout
		}
		return EngineResult{}, cctx.Err()
	case got := <-done:
		if got.err != nil {
			return EngineResult{}, got.err
		}
		if err := validateEngineResult(req, got.res); err != nil {
			return EngineResult{}, err
		}
		if got.res.Fragments <= 0 {
			got.res.Fragments = 1
		}
		if got.res.Engine == "" {
			got.res.Engine = s.cfg.Name
		}
		got.res.EngineFragment = true
		return got.res, nil
	}
}

// ---------------------------------------------------------------------------
// 降级链路：失败/超时/非法输出 → reason 并入 L1
// ---------------------------------------------------------------------------

// EngineAdapterOutcome 是一次「带开关的压缩尝试」的结果。
//   - Applied=false：调用方应保持原有 L1/L2 路径不变（开关关闭，或 L1 也失败）。
//   - Degraded=true：本次走了 L1 降级，Reason 已并入适配器失败原因；任务不失败。
//   - Fragments>0：本次压缩由引擎产出，计入 engine_fragment 统计。
type EngineAdapterOutcome struct {
	Applied      bool
	Messages     []ChatMessage
	Fragments    int
	Engine       string
	Degraded     bool
	DegradeLevel string
	Reason       string
	Degrade      DegradeResult
	TokensBefore int
	TokensAfter  int
}

// RunEngineAdapter 永不返回 error：适配器的失败、超时、非法输出都会被翻译成
// L1 降级（复用 DegradeL1），从而不阻塞、也不使任务失败。
//
// 关闭开关时直接返回 Applied=false 且 Fragments=0，调用方继续走既有单通道逻辑，
// 事件 schema 与 engine_fragment 计数均不变。
func RunEngineAdapter(ctx context.Context, slot EngineAdapterSlot, req EngineRequest, h History, limits DegradeLimits) EngineAdapterOutcome {
	engineName := slot.Config().Name
	if !slot.Enabled() {
		return EngineAdapterOutcome{Applied: false, Engine: engineName, Reason: EngineReasonDisabled}
	}

	res, err := slot.Compact(ctx, req)
	if err == nil {
		return EngineAdapterOutcome{
			Applied:      true,
			Messages:     res.Messages,
			Fragments:    res.Fragments,
			Engine:       res.Engine,
			Reason:       EngineReasonApplied,
			TokensBefore: estimateMessages(req.Messages),
			TokensAfter:  estimateMessages(res.Messages),
		}
	}

	cause := classifyEngineError(err)
	l1, l1err := DegradeL1(req.Messages, h, limits)
	if l1err != nil {
		// L1 自身都不可用（例如配对校验失败）：保持调用方原有输入，任务继续。
		return EngineAdapterOutcome{
			Applied:      false,
			Engine:       engineName,
			Degraded:     true,
			DegradeLevel: DegradeLevelL1,
			Reason:       fmt.Sprintf("%s:%s:%s", DegradeReasonEngineAdapter, cause, EngineReasonL1Failed),
		}
	}

	l1.Reason = fmt.Sprintf("%s:%s", DegradeReasonEngineAdapter, cause)
	return EngineAdapterOutcome{
		Applied:      true,
		Messages:     l1.Messages,
		Fragments:    0,
		Engine:       engineName,
		Degraded:     true,
		DegradeLevel: l1.Level,
		Reason:       l1.Reason,
		Degrade:      l1,
		TokensBefore: l1.TokensBefore,
		TokensAfter:  l1.TokensAfter,
	}
}

func classifyEngineError(err error) string {
	switch {
	case errors.Is(err, ErrEngineAdapterTimeout), errors.Is(err, context.DeadlineExceeded):
		return EngineReasonTimeout
	case errors.Is(err, ErrEngineAdapterInvalidOutput):
		return EngineReasonInvalidOutput
	default:
		return EngineReasonFailed
	}
}

// CountEngineFragments 统计 JSONL 行里产出的 engine_fragment 数量。
// phase one 中该行类型仍被 ValidateFiveElements 拒绝，因此关闭开关时必然为 0。
func CountEngineFragments(lines []Line) int {
	count := 0
	for _, line := range lines {
		if line.Type == LineEngineFragment {
			count++
		}
	}
	return count
}
