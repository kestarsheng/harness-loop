import { describe, expect, it } from 'vitest';
import type { HarnessEvent } from './types';
import type { SubtaskSnapshot } from './subtask';
import {
  buildSubtaskTree,
  computeDagProgress,
  createSubtaskSnapshot,
  reduceSubtaskState,
} from './subtask';
import type { ModelUsageState } from './modelUsage';
import {
  computeFallbackTrend,
  computeModelDistribution,
  createModelUsageState,
  reduceModelUsage,
} from './modelUsage';
import type { PolicyDenyState } from './policyDeny';
import {
  createPolicyDenyState,
  policyDenyDistribution,
  reducePolicyDenies,
  summarizeParams,
} from './policyDeny';
import {
  buildMockHandoffs,
  buildMockModelUsage,
  buildMockPolicyDenyState,
  buildMockSubtaskSnapshot,
} from './mockData';

function makeEvent(
  type: string,
  seq: number,
  overrides: Partial<HarnessEvent> = {},
): HarnessEvent {
  return {
    event_id: `evt-${type}-${seq}`,
    event_type: type,
    task_id: 'task-1',
    tenant_id: 'tenant-a',
    project_id: 'proj-a',
    seq,
    timestamp_ms: 1000 + seq * 100,
    severity: 'info',
    payload: '{}',
    ...overrides,
  };
}

function payloadOf(value: Record<string, unknown>): string {
  return JSON.stringify(value);
}

describe('reduceSubtaskState', () => {
  const queued = makeEvent('subtask.queued', 1, {
    subtask_id: 'st-api',
    dag_id: 'dag-1',
    payload: payloadOf({
      role: 'backend',
      title: '登录接口改造',
      parent_subtask_id: 'st-plan',
      parallel_set_id: 'ps-1',
    }),
  });
  const started = makeEvent('subtask.started', 2, {
    subtask_id: 'st-api',
    dag_id: 'dag-1',
    payload: payloadOf({ role: 'backend', attempt: 1 }),
  });
  const ended = makeEvent('subtask.ended', 3, {
    subtask_id: 'st-api',
    dag_id: 'dag-1',
    payload: payloadOf({ status: 'accepted', attempt: 1 }),
  });

  it('按 queued → started → ended 归约节点状态、时间与 attempt', () => {
    let state = reduceSubtaskState(undefined, queued);
    expect(state.parentTaskId).toBe('task-1');
    expect(state.dagId).toBe('dag-1');
    expect(state.nodes['st-api'].status).toBe('queued');
    expect(state.nodes['st-api'].role).toBe('backend');
    expect(state.nodes['st-api'].parentSubtaskId).toBe('st-plan');

    state = reduceSubtaskState(state, started);
    expect(state.nodes['st-api'].status).toBe('running');
    expect(state.nodes['st-api'].attempt).toBe(1);
    expect(state.nodes['st-api'].startedAtMs).toBe(started.timestamp_ms);
    expect(state.nodes['st-api'].endedAtMs).toBeNull();

    state = reduceSubtaskState(state, ended);
    expect(state.nodes['st-api'].status).toBe('accepted');
    expect(state.nodes['st-api'].endedAtMs).toBe(ended.timestamp_ms);
    expect(state.lastEventSeq).toBe(3);
  });

  it('重复 event_id 与低 seq 事件均不改变状态', () => {
    const state = reduceSubtaskState(
      reduceSubtaskState(reduceSubtaskState(undefined, queued), started),
      ended,
    );
    expect(reduceSubtaskState(state, ended)).toBe(state);

    const outOfOrder = makeEvent('subtask.started', 1, {
      event_id: 'evt-out-of-order',
      subtask_id: 'st-api',
      dag_id: 'dag-1',
    });
    expect(reduceSubtaskState(state, outOfOrder)).toBe(state);
  });

  it('failed / cancelled 记录终态与失败原因', () => {
    const failed = makeEvent('subtask.ended', 4, {
      subtask_id: 'st-api',
      dag_id: 'dag-1',
      payload: payloadOf({ status: 'failed', attempt: 2, reason: '契约测试未通过' }),
    });
    const state = reduceSubtaskState(
      reduceSubtaskState(reduceSubtaskState(undefined, queued), started),
      failed,
    );
    expect(state.nodes['st-api'].status).toBe('failed');
    expect(state.nodes['st-api'].failureReason).toBe('契约测试未通过');

    const cancelled = makeEvent('subtask.cancelled', 5, {
      subtask_id: 'st-api',
      dag_id: 'dag-1',
      payload: payloadOf({ reason: '父任务范围收敛' }),
    });
    const cancelledState = reduceSubtaskState(state, cancelled);
    expect(cancelledState.nodes['st-api'].status).toBe('cancelled');
  });

  it('归约 plan_created 与 circuit_open：无 subtask_id 也推进 seq', () => {
    const plan = makeEvent('subtask.plan_created', 1, {
      parent_task_id: 'task-1',
      dag_id: 'dag-1',
      payload: payloadOf({ node_count: 3, subtask_count: 3 }),
    });
    let state = reduceSubtaskState(undefined, plan);
    expect(state.dagId).toBe('dag-1');
    expect(state.lastEventSeq).toBe(1);
    expect(state.seenEventIds[plan.event_id]).toBe(1);
    expect(state.order).toHaveLength(0);

    const circuit = makeEvent('subtask.circuit_open', 2, {
      payload: payloadOf({ class_key: 'backend/code', failure_rate: 0.8 }),
    });
    state = reduceSubtaskState(state, circuit);
    expect(state.lastEventSeq).toBe(2);
    expect(state.seenEventIds[circuit.event_id]).toBe(2);
    expect(state.order).toHaveLength(0);
  });

  it('归约 handoff_waiting / accepted / rejected', () => {
    let state = reduceSubtaskState(undefined, queued);
    state = reduceSubtaskState(state, started);

    const waiting = makeEvent('subtask.handoff_waiting', 3, {
      subtask_id: 'st-api',
      dag_id: 'dag-1',
    });
    state = reduceSubtaskState(state, waiting);
    expect(state.nodes['st-api'].status).toBe('running');

    const accepted = makeEvent('subtask.accepted', 4, {
      subtask_id: 'st-api',
      dag_id: 'dag-1',
      payload: payloadOf({ acceptance_id: 'acc-1' }),
    });
    state = reduceSubtaskState(state, accepted);
    expect(state.nodes['st-api'].status).toBe('accepted');
    expect(state.nodes['st-api'].endedAtMs).toBe(accepted.timestamp_ms);

    const rejected = makeEvent('subtask.rejected', 5, {
      subtask_id: 'st-api',
      dag_id: 'dag-1',
      payload: payloadOf({ reason: '自验未通过' }),
    });
    state = reduceSubtaskState(state, rejected);
    expect(state.nodes['st-api'].status).toBe('failed');
    expect(state.nodes['st-api'].failureReason).toBe('自验未通过');
  });

  it('忽略非 subtask 事件与缺少 subtask_id 的事件', () => {
    const state = createSubtaskSnapshot('task-1', 'dag-1');
    expect(reduceSubtaskState(state, makeEvent('tool.started', 1))).toBe(state);

    const noId = makeEvent('subtask.started', 2, { dag_id: 'dag-1' });
    expect(reduceSubtaskState(state, noId)).toBe(state);
  });

  it('computeDagProgress 汇总状态计数与失败节点', () => {
    const root = makeEvent('subtask.ended', 1, {
      subtask_id: 'st-plan',
      dag_id: 'dag-1',
      payload: payloadOf({ status: 'accepted', role: 'planner' }),
    });
    const child = makeEvent('subtask.ended', 2, {
      subtask_id: 'st-api',
      dag_id: 'dag-1',
      payload: payloadOf({
        status: 'failed',
        role: 'backend',
        parent_subtask_id: 'st-plan',
        reason: '超时',
      }),
    });
    const grandchild = makeEvent('subtask.started', 3, {
      subtask_id: 'st-unit',
      dag_id: 'dag-1',
      payload: payloadOf({
        status: 'running',
        role: 'backend',
        parent_subtask_id: 'st-api',
        attempt: 1,
      }),
    });

    let snapshot: SubtaskSnapshot = createSubtaskSnapshot('task-1', 'dag-1');
    for (const event of [root, child, grandchild]) {
      snapshot = reduceSubtaskState(snapshot, event);
    }

    const progress = computeDagProgress(snapshot);
    expect(progress.total).toBe(3);
    expect(progress.counts.accepted).toBe(1);
    expect(progress.counts.failed).toBe(1);
    expect(progress.counts.running).toBe(1);
    expect(progress.acceptedPercent).toBe(33);
    expect(progress.failedNodes).toEqual(['st-api']);
    expect(progress.runningNodes).toEqual(['st-unit']);
  });

  it('buildSubtaskTree 按 parent_subtask_id 组装两层缩进', () => {
    const root = makeEvent('subtask.queued', 1, {
      subtask_id: 'st-plan',
      dag_id: 'dag-1',
      payload: payloadOf({ role: 'planner' }),
    });
    const child = makeEvent('subtask.queued', 2, {
      subtask_id: 'st-api',
      dag_id: 'dag-1',
      payload: payloadOf({ role: 'backend', parent_subtask_id: 'st-plan' }),
    });
    const grandchild = makeEvent('subtask.queued', 3, {
      subtask_id: 'st-unit',
      dag_id: 'dag-1',
      payload: payloadOf({ role: 'qa', parent_subtask_id: 'st-api' }),
    });

    let snapshot: SubtaskSnapshot = createSubtaskSnapshot('task-1', 'dag-1');
    for (const event of [root, child, grandchild]) {
      snapshot = reduceSubtaskState(snapshot, event);
    }

    const tree = buildSubtaskTree(snapshot, 2);
    expect(tree).toHaveLength(1);
    expect(tree[0].node.subtaskId).toBe('st-plan');
    expect(tree[0].depth).toBe(0);
    expect(tree[0].children[0].node.subtaskId).toBe('st-api');
    expect(tree[0].children[0].depth).toBe(1);
    expect(tree[0].children[0].children[0].node.subtaskId).toBe('st-unit');
    expect(tree[0].children[0].children[0].depth).toBe(2);
  });
});

describe('reduceModelUsage', () => {
  const selectedA = makeEvent('model.selected', 1, {
    stage: 'a',
    payload: payloadOf({ model: 'qwen-max', tokens: 100, cost: 0.01 }),
  });
  const fallbackB = makeEvent('model.fallback', 2, {
    stage: 'b',
    timestamp_ms: 3_500_000,
    payload: payloadOf({ from: 'qwen-plus', to: 'qwen-max', trigger: 'timeout' }),
  });
  const selectedB = makeEvent('model.selected', 3, {
    stage: 'b',
    payload: payloadOf({ model: 'qwen-max', tokens: 200, cost: 0.02 }),
  });

  const configApplied = makeEvent('model.config_applied', 5, {
    payload: payloadOf({ version: '2026.11.12-r3' }),
  });

  it('selected 写阶段生效模型、调用次数与 token/成本', () => {
    const state = reduceModelUsage(undefined, selectedA);
    expect(state.stages.a.model).toBe('qwen-max');
    expect(state.stages.a.selections).toBe(1);
    expect(state.stages.a.tokens).toBe(100);
    expect(state.stages.a.cost).toBe(0.01);
    expect(state.modelCalls['qwen-max']).toBe(1);
  });

  it('fallback 累加回退次数、切换生效模型并记录触发原因', () => {
    let state: ModelUsageState = createModelUsageState();
    state = reduceModelUsage(state, fallbackB);
    expect(state.stages.b.fallbackCount).toBe(1);
    expect(state.stages.b.model).toBe('qwen-max');
    expect(state.fallbackPoints).toHaveLength(1);
    expect(state.fallbackPoints[0].trigger).toBe('timeout');
    expect(state.fallbackPoints[0].fromModel).toBe('qwen-plus');

    state = reduceModelUsage(state, selectedB);
    expect(state.stages.b.model).toBe('qwen-max');
    expect(state.stages.b.selections).toBe(1);
    expect(state.stages.b.tokens).toBe(200);
    expect(state.modelCalls['qwen-max']).toBe(1);
  });

  it('未知 trigger 归一化为 unknown，recovered 不清零回退计数', () => {
    const weirdFallback = makeEvent('model.fallback', 6, {
      stage: 'b',
      timestamp_ms: 3_500_000,
      payload: payloadOf({ from: 'qwen-max', to: 'qwen-turbo', trigger: 'gateway-500' }),
    });
    let state = reduceModelUsage(undefined, fallbackB);
    state = reduceModelUsage(state, weirdFallback);
    expect(state.fallbackPoints.map((point) => point.trigger)).toEqual(['timeout', 'unknown']);
    expect(state.stages.b.fallbackCount).toBe(2);

    const recoveredCAfter = makeEvent('model.recovered', 7, {
      stage: 'c',
      payload: payloadOf({ model: 'qwen-plus', tokens: 50, cost: 0.005 }),
    });
    state = reduceModelUsage(state, recoveredCAfter);
    expect(state.stages.b.fallbackCount).toBe(2);
    expect(state.stages.c.model).toBe('qwen-plus');
  });

  it('config_applied 更新配置版本，其他事件被忽略', () => {
    const state = reduceModelUsage(createModelUsageState(), configApplied);
    expect(state.configVersion).toBe('2026.11.12-r3');

    const ignored = reduceModelUsage(state, makeEvent('tool.started', 9));
    expect(ignored).toBe(state);
  });

  it('归约 switched / capability_rejected / unavailable / registry_changed', () => {
    const selectedByName = makeEvent('model.selected', 1, {
      stage: 'b',
      payload: payloadOf({ model_id: 'qwen-plus', tokens: 10 }),
    });
    let state = reduceModelUsage(undefined, selectedByName);
    expect(state.stages.b.model).toBe('qwen-plus');
    expect(state.modelCalls['qwen-plus']).toBe(1);

    const switched = makeEvent('model.switched', 2, {
      stage: 'b',
      payload: payloadOf({ from: 'qwen-plus', to: 'qwen-max', trigger: 'timeout' }),
    });
    state = reduceModelUsage(state, switched);
    expect(state.stages.b.model).toBe('qwen-max');

    state = reduceModelUsage(
      state,
      makeEvent('model.capability_rejected', 3, {
        payload: payloadOf({ capability: 'vision' }),
      }),
    );
    state = reduceModelUsage(
      state,
      makeEvent('model.unavailable', 4, {
        payload: payloadOf({ model_id: 'qwen-turbo', fail_count: 3 }),
      }),
    );
    expect(state.stages.b.model).toBe('qwen-max');
    expect(state.stages.b.fallbackCount).toBe(0);

    const registry = makeEvent('model.registry_changed', 5, {
      payload: payloadOf({ version: 'v9', result: 'applied' }),
    });
    state = reduceModelUsage(state, registry);
    expect(state.configVersion).toBe('v9');
    expect(state.lastEventSeq).toBe(5);
    expect(state.seenEventIds[registry.event_id]).toBe(5);
  });

  it('重复 event_id 与低 seq 事件不改变状态', () => {
    const state = reduceModelUsage(undefined, selectedA);
    expect(reduceModelUsage(state, selectedA)).toBe(state);

    const lowSeq = makeEvent('model.selected', 1, {
      event_id: 'evt-model-low',
      stage: 'b',
      payload: payloadOf({ model: 'qwen-plus' }),
    });
    expect(reduceModelUsage(state, lowSeq)).toBe(state);
  });

  it('computeModelDistribution 按调用次数降序并截断 TopN', () => {
    const selectedC = makeEvent('model.selected', 4, {
      stage: 'c',
      payload: payloadOf({ model: 'qwen-plus' }),
    });
    let state = createModelUsageState();
    state = reduceModelUsage(state, selectedA);
    state = reduceModelUsage(state, selectedB);
    state = reduceModelUsage(state, selectedC);

    const distribution = computeModelDistribution(state, 5);
    expect(distribution[0]).toEqual({ model: 'qwen-max', calls: 2 });
    expect(distribution[1]).toEqual({ model: 'qwen-plus', calls: 1 });
    expect(computeModelDistribution(state, 1)).toHaveLength(1);
  });

  it('computeFallbackTrend 按时间桶与触发原因聚合', () => {
    let state = createModelUsageState();
    state = reduceModelUsage(state, fallbackB);
    state = reduceModelUsage(state, makeEvent('model.fallback', 7, {
      stage: 'c',
      timestamp_ms: 3_550_000,
      payload: payloadOf({ from: 'qwen-plus', to: 'qwen-max', trigger: 'rate_limit' }),
    }));

    const trend = computeFallbackTrend(state, 3_600_000, 600_000, 300_000);
    expect(trend.series.map((entry) => entry.trigger)).toEqual(['timeout', 'rate_limit']);
    const timeoutSeries = trend.series.find((entry) => entry.trigger === 'timeout');
    const rateLimitSeries = trend.series.find((entry) => entry.trigger === 'rate_limit');
    expect(timeoutSeries?.values.reduce((sum, value) => sum + value, 0)).toBe(1);
    expect(rateLimitSeries?.values.reduce((sum, value) => sum + value, 0)).toBe(1);
  });
});

describe('reducePolicyDenies', () => {
  const writeDenied = makeEvent('tool.denied', 1, {
    stage: 'c',
    payload: payloadOf({
      tool: 'write_file',
      rule_id: 'sandbox.write_allowlist',
      layer: 'sandbox',
      reason: '写入路径不在白名单',
      args: { path: '/etc/hosts' },
    }),
  });
  const pushViolation = makeEvent('security.violation', 2, {
    stage: 'c',
    payload: payloadOf({
      action: 'git_push',
      rule_id: 'permission.deny',
      reason: '禁止直接推主干',
      args: { branch: 'main' },
    }),
  });
  const writeDeniedAgain = makeEvent('tool.denied', 3, {
    stage: 'c',
    payload: payloadOf({
      tool: 'write_file',
      rule_id: 'sandbox.write_allowlist',
      layer: 'sandbox',
      reason: '写入路径不在白名单',
      args: { path: '../outside.txt' },
    }),
  });

  it('归约 tool.denied 与 security.violation，并聚合 rule/layer', () => {
    let state: PolicyDenyState = createPolicyDenyState();
    for (const event of [writeDenied, pushViolation, writeDeniedAgain]) {
      state = reducePolicyDenies(state, event);
    }

    expect(state.total).toBe(3);
    expect(state.items[0].seq).toBe(3);
    expect(state.items[1].source).toBe('security.violation');
    expect(state.items[1].layer).toBe('security');
    expect(state.byRule['sandbox.write_allowlist']).toBe(2);
    expect(state.byRule['permission.deny']).toBe(1);
    expect(state.byLayer.sandbox).toBe(2);
    expect(state.byLayer.security).toBe(1);

    const distribution = policyDenyDistribution(state);
    expect(distribution[0]).toEqual({
      ruleId: 'sandbox.write_allowlist',
      layer: 'sandbox',
      count: 2,
    });
  });

  it('重复 event_id、低 seq 与其他事件均不改变状态', () => {
    let state = reducePolicyDenies(undefined, writeDenied);
    state = reducePolicyDenies(state, pushViolation);
    expect(reducePolicyDenies(state, pushViolation)).toBe(state);

    const lowSeq = makeEvent('tool.denied', 1, {
      event_id: 'evt-deny-low',
      payload: payloadOf({ tool: 'read_file', rule_id: 'workspace.path_scope' }),
    });
    expect(reducePolicyDenies(state, lowSeq)).toBe(state);
    expect(reducePolicyDenies(state, makeEvent('tool.started', 9))).toBe(state);
  });

  it('归约 approval.* 并只把拒绝类审批计入拒绝项', () => {
    let state: PolicyDenyState = createPolicyDenyState();

    const requested = makeEvent('approval.requested', 1, {
      payload: payloadOf({
        tool: 'git_push',
        rule_id: 'permission.approval',
        code: 'approval_required',
      }),
    });
    state = reducePolicyDenies(state, requested);
    expect(state.total).toBe(0);
    expect(state.lastEventSeq).toBe(1);
    expect(state.seenEventIds[requested.event_id]).toBe(1);

    const approved = makeEvent('approval.approved', 2, {
      payload: payloadOf({ tool: 'git_push', rule_id: 'permission.approval' }),
    });
    state = reducePolicyDenies(state, approved);
    expect(state.total).toBe(0);

    const rejected = makeEvent('approval.rejected', 3, {
      payload: payloadOf({ tool: 'git_push', rule_id: 'permission.approval', code: 'E_APPROVAL_REJECTED' }),
    });
    state = reducePolicyDenies(state, rejected);
    expect(state.total).toBe(1);
    expect(state.items[0].source).toBe('approval.rejected');
    expect(state.items[0].layer).toBe('approval');
    expect(state.items[0].reason).toBe('E_APPROVAL_REJECTED');

    state = reducePolicyDenies(
      state,
      makeEvent('approval.timeout', 4, {
        payload: payloadOf({ tool: 'git_push', rule_id: 'permission.approval', code: 'E_APPROVAL_TIMEOUT' }),
      }),
    );
    state = reducePolicyDenies(
      state,
      makeEvent('approval.cancelled', 5, {
        payload: payloadOf({ tool: 'git_push', rule_id: 'permission.approval', code: 'E_APPROVAL_CANCELLED' }),
      }),
    );

    expect(state.total).toBe(3);
    expect(state.byLayer.approval).toBe(3);
    expect(state.byRule['permission.approval']).toBe(3);
  });

  it('summarizeParams 脱敏凭据字段并截断超长参数', () => {
    const summary = summarizeParams({ token: 'sk-demo-value', path: '/a' });
    expect(summary).toContain('"token":"***"');
    expect(summary).not.toContain('sk-demo-value');
    expect(summary).toContain('/a');

    const long = summarizeParams({ cmd: 'x'.repeat(300) }, 40);
    expect(long.length).toBeLessThanOrEqual(41);
    expect(long.endsWith('…')).toBe(true);
    expect(summarizeParams(null)).toBe('—');
  });
});
describe('mockData 与归约函数一致性', () => {
  const nowMs = 1_800_000_000_000;

  it('子任务快照可生成两层树与 DAG 进度', () => {
    const snapshot = buildMockSubtaskSnapshot('task-demo', nowMs);
    expect(snapshot.order).toHaveLength(7);
    expect(buildSubtaskTree(snapshot, 2)).toHaveLength(1);

    const progress = computeDagProgress(snapshot);
    expect(progress.total).toBe(7);
    expect(progress.counts.accepted).toBe(2);
    expect(progress.counts.running).toBe(2);
    expect(progress.counts.failed).toBe(1);
    expect(progress.counts.cancelled).toBe(1);
    expect(progress.counts.queued).toBe(1);
  });

  it('模型与权限 mock 快照符合页面展示预期', () => {
    const modelUsage = buildMockModelUsage(nowMs);
    expect(modelUsage.stages.c.model).toBe('qwen-max');
    expect(modelUsage.fallbackPoints).toHaveLength(2);
    expect(computeModelDistribution(modelUsage)[0].model).toBe('qwen-max');

    const policyState = buildMockPolicyDenyState(nowMs);
    expect(policyState.total).toBe(5);
    expect(policyState.byRule['sandbox.write_allowlist']).toBe(2);
    expect(policyDenyDistribution(policyState)[0].count).toBe(2);

    expect(buildMockHandoffs(nowMs)).toHaveLength(4);
  });
});
