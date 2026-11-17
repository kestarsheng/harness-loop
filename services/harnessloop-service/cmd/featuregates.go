package main

import (
	"log"
	"strings"

	"agent-platform/services/harnessloop-service/internal/config"
)

// featureGates 是 P2 三条新链路的进程级开关位（T-P2 feature flags）。
//
// 三个 bridge（OrchestratorBridge / ModelBridge / PolicyBridge）都是进程级单例，
// 构造时只吃一个 bool，所以开关必须在启动时解析一次。解析用的租户是服务自身所属
// 的租户（gateTenant）：只有 tenants 里包含它的租户规则才会在这里生效。请求级的
// 租户差异仍由 handler 对 harness.loop.fsm.enabled 逐请求求值。
type featureGates struct {
	Orchestrator  bool
	ModelStrategy bool
	PolicyLayers  bool
}

// gateTenant 是启动时求值开关所用的租户。与事件 emitter 的 EmitContext 保持一致。
const gateTenant = "platform"

// resolveFeatureGates 解析三条开关。未声明的开关一律视为关：旧行为优先，缺配置
// 不该意外打开新链路。
func resolveFeatureGates(flags *config.FlagSet) featureGates {
	ctx := config.FlagContext{TenantID: gateTenant}
	return featureGates{
		Orchestrator:  gateOn(flags, ctx, config.FlagOrchestratorEnabled),
		ModelStrategy: gateOn(flags, ctx, config.FlagModelStrategyEnabled),
		PolicyLayers:  gateOn(flags, ctx, config.FlagPolicyLayersEnabled),
	}
}

// gateOn 按「规范名 → 旧别名」的顺序求值一条开关，并把降级路径写进日志，
// 免得开关名字改了以后静默变成 false。
func gateOn(flags *config.FlagSet, ctx config.FlagContext, name string) bool {
	names := config.FlagNames(name)
	on, from := flags.EvaluateFirst(ctx, names)
	switch from {
	case "":
		log.Printf("harnessloop: flag %s not declared, keeping legacy path", name)
		return false
	case name:
		return on
	default:
		log.Printf("harnessloop: flag %s resolved from legacy name %s", name, from)
		return on
	}
}

// summary 只用于启动横幅，方便本地演示时一眼看出哪条链路亮着。
func (g featureGates) summary() string {
	mark := func(on bool) string {
		if on {
			return "on"
		}
		return "off"
	}
	parts := []string{
		"orchestrator=" + mark(g.Orchestrator),
		"model_strategy=" + mark(g.ModelStrategy),
		"policy_layers=" + mark(g.PolicyLayers),
	}
	return strings.Join(parts, " ")
}

// anyOn reports whether at least one new path is active.
func (g featureGates) anyOn() bool {
	return g.Orchestrator || g.ModelStrategy || g.PolicyLayers
}
