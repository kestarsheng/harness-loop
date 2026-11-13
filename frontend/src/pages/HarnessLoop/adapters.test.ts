import { describe, expect, it } from 'vitest';
import type { HandoffView, ModelSelectionView, SubtaskView } from './api/harnessLoop';
import {
  inferFallbackTrigger,
  normalizeSubtaskRowStatus,
  toHandoffRows,
  toModelUsageState,
  toSubtaskSnapshot,
} from './adapters';

function makeSubtask(overrides: Partial<SubtaskView> = {}): SubtaskView {
  return {
    subtask_id: 'st-a-1',
    parent_task_id: 'task-a2',
    tenant_id: 'tenant-a',
    project_id: 'proj-a',
    dag_id: 'dag-1',
    dag_node_id: 'node-a1',
    depth: 1,
    index_in_parent: 0,
    role: 'coder',
    status: 'running',
    attempt_no: 1,
    created_at_ms: 1_760_000_000_000,
    updated_at_ms: 1_760_000_010_000,
    ...overrides,
  };
}

function makeSelection(overrides: Partial<ModelSelectionView> = {}): ModelSelectionView {
  return {
    selection_id: 'sel-1',
    tenant_id: 'tenant-a',
    project_id: 'proj-a',
    task_id: 'task-a2',
    stage: 'a',
    attempt_no: 1,
    model_id: 'qwen-plus',
    strategy: 'cost_first',
    resolved_from: 'strategy',
    tokens_in: 100,
    tokens_out: 50,
    cost: 0.0002,
    is_fallback: false,
    created_at_ms: 1_760_000_000_000,
    ...overrides,
  };
}

describe('normalizeSubtaskRowStatus', () => {
  it('把 DB 枚举折叠为前端展示状态', () => {
    expect(normalizeSubtaskRowStatus('pending')).toBe('queued');
    expect(normalizeSubtaskRowStatus('running')).toBe('running');
    expect(normalizeSubtaskRowStatus('waiting_handoff')).toBe('running');
    expect(normalizeSubtaskRowStatus('accepted')).toBe('accepted');
    expect(normalizeSubtaskRowStatus('rejected')).toBe('failed');
    expect(normalizeSubtaskRowStatus('failed')).toBe('failed');
    expect(normalizeSubtaskRowStatus('cancelled')).toBe('cancelled');
  });

  it('未知状态退化为 queued', () => {
    expect(normalizeSubtaskRowStatus('unknown')).toBe('queued');
    expect(normalizeSubtaskRowStatus(undefined)).toBe('queued');
  });
});

describe('toSubtaskSnapshot', () => {
  it('按 DAG 顺序映射真实持久化行，不编造缺失字段', () => {
    const snapshot = toSubtaskSnapshot(
      [
        makeSubtask({ subtask_id: 'st-a-1', dag_node_id: 'node-a1', status: 'accepted', acceptance_id: 'acc-1' }),
        makeSubtask({
          subtask_id: 'st-a-2',
          dag_node_id: 'node-a2',
          index_in_parent: 1,
          role: 'reviewer',
          status: 'pending',
          attempt_no: 0,
        }),
        makeSubtask({
          subtask_id: 'st-b-1',
          dag_id: 'dag-2',
          dag_node_id: 'node-b1',
          status: 'waiting_handoff',
        }),
      ],
      'task-a2',
    );

    expect(snapshot.dagId).toBe('dag-1');
    expect(snapshot.parentTaskId).toBe('task-a2');
    expect(snapshot.order).toEqual(['st-a-1', 'st-a-2', 'st-b-1']);
    expect(snapshot.nodes['st-a-1'].title).toBe('node-a1');
    expect(snapshot.nodes['st-a-1'].status).toBe('accepted');
    expect(snapshot.nodes['st-a-1'].startedAtMs).toBe(1_760_000_000_000);
    expect(snapshot.nodes['st-a-1'].endedAtMs).toBe(1_760_000_010_000);
    expect(snapshot.nodes['st-a-2'].status).toBe('queued');
    expect(snapshot.nodes['st-a-2'].startedAtMs).toBe(1_760_000_000_000);
    expect(snapshot.nodes['st-a-2'].endedAtMs).toBeNull();
    expect(snapshot.nodes['st-b-1'].status).toBe('running');
    expect(snapshot.nodes['st-b-1'].parentSubtaskId).toBeNull();
    expect(snapshot.nodes['st-b-1'].failureReason).toBe('');
  });

  it('不生成后端不存在的父子关系与标题', () => {
    const snapshot = toSubtaskSnapshot([makeSubtask()], 'task-a2');
    expect(snapshot.nodes['st-a-1'].parentSubtaskId).toBeNull();
    expect(snapshot.nodes['st-a-1'].title).toBe('node-a1');
  });

  it('空数组返回空快照', () => {
    const snapshot = toSubtaskSnapshot([], 'task-a2');
    expect(snapshot.order).toEqual([]);
    expect(snapshot.nodes).toEqual({});
  });
});

describe('toModelUsageState', () => {
  it('按阶段聚合模型、token、成本与回退次数', () => {
    const state = toModelUsageState([
      makeSelection({
        selection_id: 'sel-a-1',
        stage: 'a',
        model_id: 'qwen-plus',
        tokens_in: 100,
        tokens_out: 50,
        cost: 0.0002,
        created_at_ms: 1_000,
      }),
      makeSelection({
        selection_id: 'sel-c-1',
        stage: 'c',
        model_id: 'qwen-plus',
        tokens_in: 200,
        tokens_out: 80,
        cost: 0.001,
        created_at_ms: 2_000,
      }),
      makeSelection({
        selection_id: 'sel-c-2',
        stage: 'c',
        model_id: 'qwen-max',
        tokens_in: 300,
        tokens_out: 100,
        cost: 0.002,
        is_fallback: true,
        resolved_from: 'fallback',
        reason: 'primary unavailable',
        created_at_ms: 3_000,
      }),
    ]);

    expect(state.stages.a.model).toBe('qwen-plus');
    expect(state.stages.a.selections).toBe(1);
    expect(state.stages.a.tokens).toBe(150);
    expect(state.stages.a.cost).toBeCloseTo(0.0002);

    expect(state.stages.c.model).toBe('qwen-max');
    expect(state.stages.c.selections).toBe(2);
    expect(state.stages.c.fallbackCount).toBe(1);
    expect(state.stages.c.tokens).toBe(680);
    expect(state.stages.c.cost).toBeCloseTo(0.003);
    expect(state.stages.c.lastSelectedAtMs).toBe(3_000);

    expect(state.modelCalls).toEqual({ 'qwen-plus': 2, 'qwen-max': 1 });
    expect(state.fallbackPoints).toHaveLength(1);
    expect(state.fallbackPoints[0]).toMatchObject({
      stage: 'c',
      fromModel: 'qwen-plus',
      toModel: 'qwen-max',
      trigger: 'error',
      timestampMs: 3_000,
    });
  });

  it('忽略非 a~e 阶段但仍计入模型分布', () => {
    const state = toModelUsageState([
      makeSelection({ stage: 'x', model_id: 'qwen-turbo' }),
    ]);
    expect(state.modelCalls).toEqual({ 'qwen-turbo': 1 });
    expect(state.stages.a.selections).toBe(0);
  });
});

describe('inferFallbackTrigger', () => {
  it('从真实 reason 文本推断触发原因', () => {
    expect(inferFallbackTrigger('request timeout')).toBe('timeout');
    expect(inferFallbackTrigger('rate limit 429')).toBe('rate_limit');
    expect(inferFallbackTrigger('budget exceeded')).toBe('budget');
    expect(inferFallbackTrigger('manual switch by operator')).toBe('manual');
    expect(inferFallbackTrigger('primary unavailable')).toBe('error');
    expect(inferFallbackTrigger('')).toBe('unknown');
  });
});

describe('toHandoffRows', () => {
  const rows: HandoffView[] = [
    {
      handoff_id: 'ho-old',
      parent_task_id: 'task-a2',
      subtask_id: 'st-a-1',
      from_agent_id: 'agent-a',
      to_agent_id: 'agent-b',
      status: 'created',
      reason: '',
      input_digest: 'sha256:old',
      created_at_ms: 1_000,
      updated_at_ms: 1_000,
    },
    {
      handoff_id: 'ho-new',
      parent_task_id: 'task-a2',
      subtask_id: 'st-a-2',
      from_agent_id: 'agent-b',
      to_agent_id: 'agent-c',
      status: 'accepted',
      reason: 'ok',
      input_digest: 'sha256:new',
      created_at_ms: 2_000,
      updated_at_ms: 2_500,
    },
  ];

  it('映射真实字段并按时间倒序，不补 role', () => {
    const result = toHandoffRows(rows);
    expect(result).toHaveLength(2);
    expect(result[0].handoffId).toBe('ho-new');
    expect(result[0]).toMatchObject({
      parentTaskId: 'task-a2',
      subtaskId: 'st-a-2',
      fromAgentId: 'agent-b',
      toAgentId: 'agent-c',
      status: 'accepted',
      reason: 'ok',
      inputDigest: 'sha256:new',
      createdAtMs: 2_000,
      updatedAtMs: 2_500,
    });
    expect(result[0]).not.toHaveProperty('fromRole');
    expect(result[0]).not.toHaveProperty('toRole');
    expect(result[0]).not.toHaveProperty('from_role');
    expect(result[0]).not.toHaveProperty('to_role');
  });

  it('缺失字段保持为空字符串，由 UI 显示占位符', () => {
    const result = toHandoffRows([
      { handoff_id: 'ho-empty', status: 'created' },
    ]);
    expect(result[0]).toMatchObject({
      parentTaskId: '',
      subtaskId: '',
      fromAgentId: '',
      toAgentId: '',
      reason: '',
      inputDigest: '',
      createdAtMs: 0,
    });
  });
});