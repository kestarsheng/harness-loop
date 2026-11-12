import type { HarnessEvent } from './types';
import { parsePayload, toNumber } from './types';

export const SUBTASK_STATUSES = ['queued', 'running', 'accepted', 'failed', 'cancelled'] as const;

export type SubtaskStatus = (typeof SUBTASK_STATUSES)[number];

export const SUBTASK_STATUS_LABELS: Record<SubtaskStatus, string> = {
  queued: '排队中',
  running: '执行中',
  accepted: '已验收',
  failed: '失败',
  cancelled: '已取消',
};

export interface SubtaskRuntime {
  subtaskId: string;
  parentTaskId: string;
  parentSubtaskId: string | null;
  dagId: string;
  role: string;
  title: string;
  status: SubtaskStatus;
  attempt: number;
  startedAtMs: number | null;
  endedAtMs: number | null;
  parallelSetId: string | null;
  failureReason: string;
  lastEventSeq: number;
}

export interface SubtaskSnapshot {
  dagId: string;
  parentTaskId: string;
  nodes: Record<string, SubtaskRuntime>;
  order: string[];
  lastEventSeq: number;
  seenEventIds: Record<string, number>;
}

export interface SubtaskTreeNode {
  node: SubtaskRuntime;
  depth: number;
  children: SubtaskTreeNode[];
}

export interface DagProgress {
  dagId: string;
  total: number;
  counts: Record<SubtaskStatus, number>;
  acceptedPercent: number;
  failedNodes: string[];
  runningNodes: string[];
}

export interface HandoffContractSummary {
  handoffId: string;
  fromRole: string;
  toRole: string;
  scope: string;
  artifactCount: number;
  acceptedCriteria: string[];
  createdAtMs: number;
}

const SUBTASK_EVENT_PREFIX = 'subtask.';

export function createSubtaskSnapshot(
  parentTaskId = '',
  dagId = '',
): SubtaskSnapshot {
  return {
    dagId,
    parentTaskId,
    nodes: {},
    order: [],
    lastEventSeq: 0,
    seenEventIds: {},
  };
}

export function normalizeSubtaskStatus(value: unknown): SubtaskStatus {
  const text = typeof value === 'string' ? value.trim().toLowerCase() : '';
  return (SUBTASK_STATUSES as readonly string[]).includes(text)
    ? (text as SubtaskStatus)
    : 'queued';
}

function payloadString(payload: Record<string, unknown>, keys: string[]): string {
  for (const key of keys) {
    const value = payload[key];
    if (typeof value === 'string' && value.trim() !== '') {
      return value.trim();
    }
  }
  return '';
}

function payloadNumber(payload: Record<string, unknown>, keys: string[], fallback = 0): number {
  for (const key of keys) {
    if (payload[key] !== undefined && payload[key] !== null) {
      return toNumber(payload[key], fallback);
    }
  }
  return fallback;
}

function endedStatus(payload: Record<string, unknown>): SubtaskStatus {
  const normalized = normalizeSubtaskStatus(payload.status ?? payload.outcome);
  if (normalized === 'accepted' || normalized === 'failed' || normalized === 'cancelled') {
    return normalized;
  }
  return payload.passed === false ? 'failed' : 'accepted';
}

export function reduceSubtaskState(
  previous: SubtaskSnapshot | undefined,
  event: HarnessEvent,
): SubtaskSnapshot {
  const base =
    previous ?? createSubtaskSnapshot(event.parent_task_id || event.task_id || '', event.dag_id || '');

  if (!event.event_id) {
    return base;
  }
  if (Object.prototype.hasOwnProperty.call(base.seenEventIds, event.event_id)) {
    return base;
  }
  if (!event.event_type.startsWith(SUBTASK_EVENT_PREFIX)) {
    return base;
  }

  const seq = toNumber(event.seq);
  if (previous && seq <= previous.lastEventSeq) {
    return previous;
  }

  const payload = parsePayload(event.payload);

  if (event.event_type === 'subtask.plan_created' || event.event_type === 'subtask.circuit_open') {
    const planDagId = event.dag_id || payloadString(payload, ['dag_id', 'dagId']) || base.dagId;
    const planParentTaskId =
      event.parent_task_id || payloadString(payload, ['parent_task_id']) || base.parentTaskId;
    return {
      ...base,
      dagId: planDagId,
      parentTaskId: planParentTaskId,
      lastEventSeq: Math.max(base.lastEventSeq, seq),
      seenEventIds: { ...base.seenEventIds, [event.event_id]: seq },
    };
  }

  const subtaskId =
    event.subtask_id || payloadString(payload, ['subtask_id', 'subtaskId', 'id']);
  if (!subtaskId) {
    return base;
  }

  const dagId = event.dag_id || payloadString(payload, ['dag_id', 'dagId']) || base.dagId;
  const parentTaskId =
    event.parent_task_id || payloadString(payload, ['parent_task_id']) || base.parentTaskId;
  const parentSubtaskId =
    payloadString(payload, ['parent_subtask_id', 'parentSubtaskId']) ||
    base.nodes[subtaskId]?.parentSubtaskId ||
    null;

  const existing = base.nodes[subtaskId];
  const node: SubtaskRuntime = existing
    ? { ...existing }
    : {
        subtaskId,
        parentTaskId,
        parentSubtaskId,
        dagId,
        role: 'worker',
        title: '',
        status: 'queued',
        attempt: 0,
        startedAtMs: null,
        endedAtMs: null,
        parallelSetId: null,
        failureReason: '',
        lastEventSeq: seq,
      };

  node.parentTaskId = parentTaskId || node.parentTaskId;
  node.parentSubtaskId = parentSubtaskId;
  node.dagId = dagId || node.dagId;
  node.lastEventSeq = seq;

  const role = payloadString(payload, ['role', 'agent_role']);
  if (role) {
    node.role = role;
  }
  const title = payloadString(payload, ['title', 'goal', 'summary']);
  if (title) {
    node.title = title;
  }
  const parallelSetId = payloadString(payload, ['parallel_set_id', 'parallelSetId']);
  if (parallelSetId) {
    node.parallelSetId = parallelSetId;
  }

  const timestampMs = toNumber(event.timestamp_ms);
  const attemptNo = payloadNumber(payload, ['attempt', 'attempt_no'], 0);

  switch (event.event_type) {
    case 'subtask.queued':
      node.status = 'queued';
      if (attemptNo > 0) {
        node.attempt = attemptNo;
      }
      break;
    case 'subtask.started':
      node.status = 'running';
      node.startedAtMs = timestampMs;
      node.endedAtMs = null;
      node.attempt = attemptNo > 0 ? attemptNo : Math.max(1, node.attempt || 1);
      break;
    case 'subtask.handoff_waiting':
      if (node.status !== 'accepted' && node.status !== 'failed' && node.status !== 'cancelled') {
        node.status = 'running';
      }
      break;
    case 'subtask.accepted':
      node.status = 'accepted';
      node.endedAtMs = timestampMs;
      break;
    case 'subtask.rejected':
      node.status = 'failed';
      node.endedAtMs = timestampMs;
      node.failureReason = payloadString(payload, ['reason', 'failure_reason']) || '自验未通过';
      break;
    case 'subtask.ended': {
      node.status = endedStatus(payload);
      node.endedAtMs = timestampMs;
      if (attemptNo > 0) {
        node.attempt = attemptNo;
      }
      if (node.status === 'failed') {
        node.failureReason = payloadString(payload, ['reason', 'failure_reason']) || '子任务失败';
      }
      break;
    }
    case 'subtask.failed':
      node.status = 'failed';
      node.endedAtMs = timestampMs;
      node.failureReason = payloadString(payload, ['reason', 'failure_reason']) || '子任务失败';
      break;
    case 'subtask.cancelled':
      node.status = 'cancelled';
      node.endedAtMs = timestampMs;
      break;
    default:
      break;
  }

  const order = existing ? base.order : [...base.order, subtaskId];

  return {
    dagId: dagId || base.dagId,
    parentTaskId: parentTaskId || base.parentTaskId,
    nodes: { ...base.nodes, [subtaskId]: node },
    order,
    lastEventSeq: Math.max(base.lastEventSeq, seq),
    seenEventIds: { ...base.seenEventIds, [event.event_id]: seq },
  };
}

export function computeDagProgress(snapshot: SubtaskSnapshot): DagProgress {
  const counts: Record<SubtaskStatus, number> = {
    queued: 0,
    running: 0,
    accepted: 0,
    failed: 0,
    cancelled: 0,
  };
  const failedNodes: string[] = [];
  const runningNodes: string[] = [];

  for (const subtaskId of snapshot.order) {
    const node = snapshot.nodes[subtaskId];
    if (!node) {
      continue;
    }
    counts[node.status] += 1;
    if (node.status === 'failed') {
      failedNodes.push(subtaskId);
    } else if (node.status === 'running') {
      runningNodes.push(subtaskId);
    }
  }

  const total =
    counts.queued + counts.running + counts.accepted + counts.failed + counts.cancelled;

  return {
    dagId: snapshot.dagId,
    total,
    counts,
    acceptedPercent: total > 0 ? Math.round((counts.accepted / total) * 100) : 0,
    failedNodes,
    runningNodes,
  };
}

export function buildSubtaskTree(
  snapshot: SubtaskSnapshot,
  maxDepth = 2,
): SubtaskTreeNode[] {
  const childrenByParent = new Map<string, SubtaskRuntime[]>();
  const roots: SubtaskRuntime[] = [];

  for (const subtaskId of snapshot.order) {
    const node = snapshot.nodes[subtaskId];
    if (!node) {
      continue;
    }
    const parentId = node.parentSubtaskId;
    if (!parentId || !snapshot.nodes[parentId]) {
      roots.push(node);
      continue;
    }
    const bucket = childrenByParent.get(parentId);
    if (bucket) {
      bucket.push(node);
    } else {
      childrenByParent.set(parentId, [node]);
    }
  }

  const walk = (node: SubtaskRuntime, depth: number): SubtaskTreeNode => {
    const children =
      depth < maxDepth
        ? (childrenByParent.get(node.subtaskId) ?? []).map((child) => walk(child, depth + 1))
        : [];
    return { node, depth, children };
  };

  return roots.map((node) => walk(node, 0));
}