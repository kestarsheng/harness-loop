import type { HandoffView, ModelSelectionView, SubtaskView } from './api/harnessLoop';
import type { SubtaskSnapshot, SubtaskStatus } from './subtask';
import { createSubtaskSnapshot } from './subtask';
import type {
  ModelFallbackPoint,
  ModelFallbackTrigger,
  ModelStageUsage,
  ModelUsageState,
} from './modelUsage';
import { createModelUsageState } from './modelUsage';
import { normalizeStage, toNumber } from './types';
import type { StageCode } from './types';

// hlp_task.subtasks.status 的枚举值与前端展示状态不同：
// pending → 排队，waiting_handoff/rejected 需要折叠到前端已有的状态集合。
const SUBTASK_STATUS_FROM_DB: Record<string, SubtaskStatus> = {
  pending: 'queued',
  queued: 'queued',
  running: 'running',
  waiting_handoff: 'running',
  accepted: 'accepted',
  rejected: 'failed',
  failed: 'failed',
  cancelled: 'cancelled',
};

const TERMINAL_SUBTASK_STATUSES: readonly SubtaskStatus[] = ['accepted', 'failed', 'cancelled'];

export function normalizeSubtaskRowStatus(value: unknown): SubtaskStatus {
  const key = typeof value === 'string' ? value.trim().toLowerCase() : '';
  return SUBTASK_STATUS_FROM_DB[key] ?? 'queued';
}

/**
 * 把 hlp_task.subtasks 的持久化行还原成 DAG 视图。
 * 行内没有 parent_subtask_id / title / started_at 列：
 * - title 使用 dag_node_id（后端生成，稳定且可读）
 * - startedAt 使用 created_at，endedAt 仅在终态时使用 updated_at
 * 不为缺失字段编造数据。
 */
export function toSubtaskSnapshot(
  views: SubtaskView[],
  parentTaskId: string,
): SubtaskSnapshot {
  const snapshot = createSubtaskSnapshot(parentTaskId, '');
  const nodes: SubtaskSnapshot['nodes'] = {};
  const order: string[] = [];
  let dagId = '';

  for (const view of views) {
    if (!view || !view.subtask_id) {
      continue;
    }
    const status = normalizeSubtaskRowStatus(view.status);
    const createdAtMs = toNumber(view.created_at_ms) || null;
    const updatedAtMs = toNumber(view.updated_at_ms) || null;
    const endedAtMs =
      TERMINAL_SUBTASK_STATUSES.includes(status) && updatedAtMs !== null ? updatedAtMs : null;
    if (!dagId && view.dag_id) {
      dagId = view.dag_id;
    }
    if (!order.includes(view.subtask_id)) {
      order.push(view.subtask_id);
    }
    nodes[view.subtask_id] = {
      subtaskId: view.subtask_id,
      parentTaskId: view.parent_task_id || parentTaskId,
      parentSubtaskId: null,
      dagId: view.dag_id || '',
      role: view.role || 'worker',
      title: view.dag_node_id || view.subtask_id,
      status,
      attempt: toNumber(view.attempt_no),
      startedAtMs: createdAtMs,
      endedAtMs,
      parallelSetId: view.parallel_set_id || null,
      failureReason: '',
      lastEventSeq: 0,
    };
  }

  return {
    ...snapshot,
    dagId,
    nodes,
    order,
  };
}

export function inferFallbackTrigger(reason: unknown): ModelFallbackTrigger {
  const text = typeof reason === 'string' ? reason.trim().toLowerCase() : '';
  if (text.includes('timeout') || text.includes('timed out')) {
    return 'timeout';
  }
  if (text.includes('rate') || text.includes('limit') || text.includes('429')) {
    return 'rate_limit';
  }
  if (text.includes('budget') || text.includes('quota') || text.includes('cost')) {
    return 'budget';
  }
  if (text.includes('manual') || text.includes('operator')) {
    return 'manual';
  }
  if (text.includes('unavailable') || text.includes('error') || text.includes('fail')) {
    return 'error';
  }
  return 'unknown';
}

function emptyStageUsage(stage: StageCode): ModelStageUsage {
  return {
    stage,
    model: '',
    selections: 0,
    fallbackCount: 0,
    tokens: 0,
    cost: 0,
    lastSelectedAtMs: 0,
  };
}

/**
 * hlp_task.model_selections 按时间升序回放，聚合成每阶段与模型分布。
 * fallback 的 fromModel 取同阶段上一条真实 selection 的 model_id（数据库无显式 from 列）。
 */
export function toModelUsageState(views: ModelSelectionView[]): ModelUsageState {
  const base = createModelUsageState();
  const stages: Record<StageCode, ModelStageUsage> = {
    a: emptyStageUsage('a'),
    b: emptyStageUsage('b'),
    c: emptyStageUsage('c'),
    d: emptyStageUsage('d'),
    e: emptyStageUsage('e'),
  };
  const modelCalls: Record<string, number> = {};
  const fallbackPoints: ModelFallbackPoint[] = [];
  const lastModelByStage: Partial<Record<StageCode, string>> = {};

  const ordered = [...views]
    .filter((view) => view && view.selection_id)
    .sort((left, right) => toNumber(left.created_at_ms) - toNumber(right.created_at_ms));

  for (const view of ordered) {
    const stage = normalizeStage(view.stage);
    const model = typeof view.model_id === 'string' ? view.model_id.trim() : '';
    if (!model) {
      continue;
    }
    const timestampMs = toNumber(view.created_at_ms);
    const tokens = toNumber(view.tokens_in) + toNumber(view.tokens_out);
    const cost = toNumber(view.cost);
    modelCalls[model] = (modelCalls[model] ?? 0) + 1;

    if (!stage) {
      continue;
    }

    const usage = stages[stage];
    const isFallback = Boolean(view.is_fallback);
    if (isFallback) {
      fallbackPoints.push({
        subtaskOrStage: view.selection_id,
        stage,
        fromModel: lastModelByStage[stage] ?? '',
        toModel: model,
        trigger: inferFallbackTrigger(view.reason),
        timestampMs,
      });
    }
    stages[stage] = {
      ...usage,
      model,
      selections: usage.selections + 1,
      fallbackCount: usage.fallbackCount + (isFallback ? 1 : 0),
      tokens: usage.tokens + tokens,
      cost: usage.cost + cost,
      lastSelectedAtMs: Math.max(usage.lastSelectedAtMs, timestampMs),
    };
    lastModelByStage[stage] = model;
  }

  fallbackPoints.sort((left, right) => left.timestampMs - right.timestampMs);

  return {
    ...base,
    stages,
    modelCalls,
    fallbackPoints,
    configVersion: '',
  };
}

/**
 * /handoffs 返回的是后端把 handoff.* 事件折叠后的行（无独立表、无 role 字段）。
 * 这里只保留真实存在的字段，不补 role。
 */
export interface HandoffRowView {
  handoffId: string;
  parentTaskId: string;
  subtaskId: string;
  fromAgentId: string;
  toAgentId: string;
  status: string;
  reason: string;
  inputDigest: string;
  createdAtMs: number;
  updatedAtMs: number;
}

export function toHandoffRows(views: HandoffView[]): HandoffRowView[] {
  return views
    .filter((view) => view && view.handoff_id)
    .map((view) => ({
      handoffId: view.handoff_id,
      parentTaskId: view.parent_task_id || '',
      subtaskId: view.subtask_id || '',
      fromAgentId: view.from_agent_id || '',
      toAgentId: view.to_agent_id || '',
      status: view.status || 'created',
      reason: view.reason || '',
      inputDigest: view.input_digest || '',
      createdAtMs: toNumber(view.created_at_ms),
      updatedAtMs: toNumber(view.updated_at_ms),
    }))
    .sort((left, right) => right.createdAtMs - left.createdAtMs);
}

