export const STAGES = ['a', 'b', 'c', 'd', 'e'] as const;

export type StageCode = (typeof STAGES)[number];

export const STAGE_LABELS: Record<StageCode, string> = {
  a: '需求澄清',
  b: '方案规划',
  c: '执行实现',
  d: '验收汇聚',
  e: '交付归档',
};

export const KNOWN_EVENT_TYPES = [
  'task.started',
  'task.completed',
  'task.aborted',
  'task.failed',
  'task.paused',
  'task.resumed',
  'stage.entered',
  'stage.exited',
  'stage.transition',
  'stage.illegal_transition',
  'invalid_finish',
  'acceptance.started',
  'acceptance.passed',
  'acceptance.failed',
  'acceptance.invalid',
  'acceptance.rollback_overridden',
  'retry.scheduled',
  'retry.exhausted',
  'checkpoint.saved',
  'checkpoint.restored',
  'checkpoint.drift',
  'checkpoint.corrupted',
  'compaction.triggered',
  'compaction.completed',
  'compaction.degraded',
  'compaction.rejected',
  'memory.written',
  'memory.merged',
  'memory.promoted',
  'memory.evicted',
  'memory.decayed',
  'memory.corrupted',
  'memory.resync',
  'memory.index_degraded',
  'index.queued',
  'index.updated',
  'index.failed',
  'index.rebuilt',
  'tool.started',
  'tool.result',
  'tool.progress',
  'tool.denied',
  'subtask.plan_created',
  'subtask.queued',
  'subtask.started',
  'subtask.handoff_waiting',
  'subtask.accepted',
  'subtask.rejected',
  'subtask.failed',
  'subtask.cancelled',
  'subtask.circuit_open',
  'handoff.created',
  'handoff.accepted',
  'handoff.rejected',
  'aggregation.completed',
  'aggregation.conflict',
  'model.selected',
  'model.switched',
  'model.fallback',
  'model.capability_rejected',
  'model.recovered',
  'model.unavailable',
  'model.registry_changed',
  'model.config_applied',
  'approval.requested',
  'approval.approved',
  'approval.rejected',
  'approval.timeout',
  'approval.cancelled',
  'error',
  'security.violation',
  'state.rebuild',
  'event.invalid',
] as const;

export type HarnessEventType = (typeof KNOWN_EVENT_TYPES)[number];

export type Severity = 'debug' | 'info' | 'warn' | 'error';

export type TaskStatus =
  | 'pending'
  | 'running'
  | 'waiting_acceptance'
  | 'retrying'
  | 'paused'
  | 'completed'
  | 'failed'
  | 'aborted';

export type Verdict = 'passed' | 'failed' | 'invalid' | null;

export interface HarnessEvent {
  event_id: string;
  event_type: HarnessEventType | string;
  schema_version?: string;
  task_id: string;
  trace_id?: string;
  span_id?: string;
  stage?: string | null;
  attempt?: number | string | null;
  seq: number | string;
  timestamp_ms: number | string;
  severity?: Severity | string;
  tenant_id?: string;
  project_id?: string;
  agent_id?: string;
  payload?: string | Record<string, unknown> | null;
  payload_ref?: string;
  parent_task_id?: string;
  subtask_id?: string;
  handoff_id?: string;
  dag_id?: string;
  dag_node_id?: string;
}

export interface HarnessTaskView {
  task_id: string;
  tenant_id?: string;
  project_id?: string;
  workspace_root?: string;
  goal?: string;
  goal_hash?: string;
  current_stage?: string;
  status?: string;
  terminal_reason?: string;
  attempt_no_by_stage?: Record<string, number>;
  retry_count?: number;
  acceptance_fail_count?: number;
  budget?: { tokens?: number; cost?: number; wall_clock_seconds?: number };
  consumed_tokens?: number;
  consumed_cost?: number;
  fsm_enabled?: boolean;
  version?: number;
  created_at_ms?: number | string;
  updated_at_ms?: number | string;
  last_checkpoint_id?: string;
}

export interface CheckpointView {
  checkpoint_id: string;
  task_id: string;
  kind?: string;
  stage?: string;
  attempt?: number;
  created_at_ms?: number | string;
  event_seq_watermark?: number | string;
  payload_ref?: string;
}

export interface StageRuntime {
  attempts: number;
  enteredAtMs: number | null;
  exitedAtMs: number | null;
  durationMs: number;
  lastAttempt: number;
}

export interface AnomalyRef {
  eventId: string;
  traceId: string;
  seq: number;
  eventType: string;
  message: string;
  stage: string | null;
  timestampMs: number;
}

export interface TaskRuntimeState {
  taskId: string;
  tenantId: string;
  projectId: string;
  agentId: string;
  goal: string;
  stage: StageCode | null;
  status: TaskStatus;
  terminalReason: string;
  attemptNoByStage: Record<StageCode, number>;
  attempts: number;
  retryCount: number;
  acceptanceTotal: number;
  acceptancePassed: number;
  acceptanceFailed: number;
  recentVerdict: Verdict;
  errorCount: number;
  securityViolations: number;
  toolCalls: number;
  eventCount: number;
  eventsInWindow: number;
  invalidEvents: number;
  anomalies: AnomalyRef[];
  stages: Record<StageCode, StageRuntime>;
  transitions: Array<{ from: string; to: string; seq: number }>;
  lastCheckpointId: string;
  lastError: AnomalyRef | null;
  startedAtMs: number | null;
  updatedAtMs: number;
  lastEventSeq: number;
  eventIds: Record<string, number>;
  recentEvents: HarnessEvent[];
  compactionCount: number;
  degradedCount: number;
  memoryEventCount: number;
  indexStatus: string;
  healthScore: number;
}

export interface HealthBreakdown {
  score: number;
  acceptancePassRate: number;
  retryRate: number;
  stageFreshness: number;
  errorDensity: number;
  sampleInsufficient: boolean;
}

export const HEALTH_WEIGHTS = {
  acceptance: 0.35,
  retry: 0.25,
  freshness: 0.2,
  error: 0.2,
};

export const DEFAULT_STAGE_BUDGET_MS = 30 * 60 * 1000;
export const MAX_RECENT_EVENTS = 200;

const TERMINAL_STATUSES: TaskStatus[] = ['completed', 'failed', 'aborted'];

export function isTerminalStatus(status: TaskStatus): boolean {
  return TERMINAL_STATUSES.includes(status);
}

export function toNumber(value: unknown, fallback = 0): number {
  if (typeof value === 'number') {
    return Number.isFinite(value) ? value : fallback;
  }
  if (typeof value === 'string' && value.trim() !== '') {
    const parsed = Number(value);
    return Number.isFinite(parsed) ? parsed : fallback;
  }
  return fallback;
}

export function normalizeStage(value: unknown): StageCode | null {
  if (typeof value !== 'string') {
    return null;
  }
  const stage = value.trim().toLowerCase();
  return (STAGES as readonly string[]).includes(stage) ? (stage as StageCode) : null;
}

function decodeBase64Utf8(raw: string): string {
  const binary = atob(raw);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) {
    bytes[i] = binary.charCodeAt(i);
  }
  return new TextDecoder().decode(bytes);
}

export function parsePayload(payload: unknown): Record<string, unknown> {
  if (!payload) {
    return {};
  }
  if (typeof payload === 'object') {
    return payload as Record<string, unknown>;
  }
  if (typeof payload !== 'string') {
    return {};
  }
  let text = payload.trim();
  if (text === '') {
    return {};
  }
  if (text.startsWith('{') || text.startsWith('[')) {
    try {
      const parsed = JSON.parse(text);
      return typeof parsed === 'object' && parsed !== null ? (parsed as Record<string, unknown>) : {};
    } catch {
      return {};
    }
  }
  try {
    text = decodeBase64Utf8(text);
    const parsed = JSON.parse(text);
    return typeof parsed === 'object' && parsed !== null ? (parsed as Record<string, unknown>) : {};
  } catch {
    return {};
  }
}

export function createEmptyStageRuntime(): StageRuntime {
  return { attempts: 0, enteredAtMs: null, exitedAtMs: null, durationMs: 0, lastAttempt: 0 };
}

export function createEmptyStageMap(): Record<StageCode, StageRuntime> {
  return {
    a: createEmptyStageRuntime(),
    b: createEmptyStageRuntime(),
    c: createEmptyStageRuntime(),
    d: createEmptyStageRuntime(),
    e: createEmptyStageRuntime(),
  };
}

export function createTaskState(taskId: string, partial?: Partial<TaskRuntimeState>): TaskRuntimeState {
  return {
    taskId,
    tenantId: '',
    projectId: '',
    agentId: '',
    goal: '',
    stage: null,
    status: 'pending',
    terminalReason: '',
    attemptNoByStage: { a: 0, b: 0, c: 0, d: 0, e: 0 },
    attempts: 0,
    retryCount: 0,
    acceptanceTotal: 0,
    acceptancePassed: 0,
    acceptanceFailed: 0,
    recentVerdict: null,
    errorCount: 0,
    securityViolations: 0,
    toolCalls: 0,
    eventCount: 0,
    eventsInWindow: 0,
    invalidEvents: 0,
    anomalies: [],
    stages: createEmptyStageMap(),
    transitions: [],
    lastCheckpointId: '',
    lastError: null,
    startedAtMs: null,
    updatedAtMs: 0,
    lastEventSeq: 0,
    eventIds: {},
    recentEvents: [],
    compactionCount: 0,
    degradedCount: 0,
    memoryEventCount: 0,
    indexStatus: '',
    healthScore: 100,
    ...partial,
  };
}

function pushAnomaly(state: TaskRuntimeState, event: HarnessEvent, message: string): AnomalyRef[] {
  const ref: AnomalyRef = {
    eventId: event.event_id,
    traceId: event.trace_id || '',
    seq: toNumber(event.seq),
    eventType: event.event_type,
    message,
    stage: event.stage || null,
    timestampMs: toNumber(event.timestamp_ms),
  };
  const next = [ref, ...state.anomalies];
  return next.slice(0, 50);
}

function applyStageAttempt(state: TaskRuntimeState, stage: StageCode, attemptNo: number) {
  const stageState = state.stages[stage];
  state.stages = {
    ...state.stages,
    [stage]: {
      ...stageState,
      attempts: stageState.attempts + 1,
      lastAttempt: attemptNo || stageState.lastAttempt + 1,
      enteredAtMs: state.updatedAtMs,
      exitedAtMs: null,
    },
  };
  state.attemptNoByStage = {
    ...state.attemptNoByStage,
    [stage]: Math.max(state.attemptNoByStage[stage], attemptNo || 0),
  };
  state.attempts += 1;
}

export function computeHealth(
  state: TaskRuntimeState,
  now: number = Date.now(),
  stageBudgetMs: number = DEFAULT_STAGE_BUDGET_MS,
): HealthBreakdown {
  const sampleInsufficient = state.acceptanceTotal === 0;
  const acceptancePassRate = sampleInsufficient
    ? 0.5
    : state.acceptancePassed / Math.max(1, state.acceptanceTotal);
  const retryRate = state.retryCount / Math.max(1, state.attempts);

  let stageFreshness = 1;
  if (!isTerminalStatus(state.status) && state.stage) {
    const enteredAt = state.stages[state.stage]?.enteredAtMs ?? state.updatedAtMs;
    const dwell = Math.max(0, now - enteredAt);
    stageFreshness = Math.min(1, Math.max(0, 1 - dwell / Math.max(1, stageBudgetMs)));
  }

  const errorDensity = state.errorCount / Math.max(1, state.eventsInWindow);

  const score =
    100 *
    (HEALTH_WEIGHTS.acceptance * acceptancePassRate +
      HEALTH_WEIGHTS.retry * (1 - retryRate) +
      HEALTH_WEIGHTS.freshness * stageFreshness +
      HEALTH_WEIGHTS.error * (1 - Math.min(1, errorDensity)));

  return {
    score: Math.max(0, Math.min(100, Math.round(score))),
    acceptancePassRate,
    retryRate: Math.min(1, retryRate),
    stageFreshness,
    errorDensity: Math.min(1, errorDensity),
    sampleInsufficient,
  };
}

export function healthLevel(score: number): 'danger' | 'warn' | 'ok' {
  if (score < 40) {
    return 'danger';
  }
  if (score <= 70) {
    return 'warn';
  }
  return 'ok';
}

function isDuplicate(state: TaskRuntimeState, event: HarnessEvent): boolean {
  return Object.prototype.hasOwnProperty.call(state.eventIds, event.event_id);
}

export function reduceTaskState(
  previous: TaskRuntimeState | undefined,
  event: HarnessEvent,
): TaskRuntimeState {
  const seq = toNumber(event.seq);
  const timestampMs = toNumber(event.timestamp_ms);
  const base = previous ?? createTaskState(event.task_id || '');

  if (!event.event_id || isDuplicate(base, event)) {
    return base;
  }
  if (previous && seq <= previous.lastEventSeq) {
    return previous;
  }

  const payload = parsePayload(event.payload);
  const stage = normalizeStage(event.stage);
  const attemptNo = toNumber(event.attempt);

  const state: TaskRuntimeState = {
    ...base,
    taskId: event.task_id || base.taskId,
    tenantId: event.tenant_id || base.tenantId,
    projectId: event.project_id || base.projectId,
    agentId: event.agent_id || base.agentId,
    eventCount: base.eventCount + 1,
    eventsInWindow: base.eventsInWindow + 1,
    updatedAtMs: Math.max(base.updatedAtMs, timestampMs),
    lastEventSeq: Math.max(base.lastEventSeq, seq),
    eventIds: { ...base.eventIds, [event.event_id]: seq },
    recentEvents: [...base.recentEvents, event].slice(-MAX_RECENT_EVENTS),
    anomalies: base.anomalies,
    stages: { ...base.stages, ...createEmptyStageMap(), ...base.stages },
    attemptNoByStage: { ...base.attemptNoByStage },
    transitions: base.transitions,
  };

  switch (event.event_type) {
    case 'task.started': {
      state.status = 'running';
      state.startedAtMs = base.startedAtMs ?? timestampMs;
      state.terminalReason = '';
      state.stage = stage ?? state.stage;
      if (payload.goal && typeof payload.goal === 'string') {
        state.goal = payload.goal;
      }
      break;
    }
    case 'task.completed':
      state.status = 'completed';
      state.terminalReason = String(payload.terminal_reason ?? 'completed');
      break;
    case 'task.aborted':
      state.status = 'aborted';
      state.terminalReason = String(payload.terminal_reason ?? 'aborted');
      break;
    case 'task.failed':
      state.status = 'failed';
      state.terminalReason = String(payload.terminal_reason ?? payload.reason ?? 'failed');
      state.lastError = {
        eventId: event.event_id,
        traceId: event.trace_id || '',
        seq,
        eventType: event.event_type,
        message: String(payload.reason ?? payload.terminal_reason ?? 'failed'),
        stage: event.stage || null,
        timestampMs,
      };
      break;
    case 'task.paused':
      state.status = 'paused';
      break;
    case 'task.resumed':
      state.status = 'running';
      break;
    case 'stage.entered': {
      const enteredStage = stage ?? state.stage;
      if (enteredStage) {
        state.stage = enteredStage;
        applyStageAttempt(state, enteredStage, attemptNo);
      }
      if (!isTerminalStatus(state.status)) {
        state.status = 'running';
      }
      break;
    }
    case 'stage.exited': {
      const exitedStage = stage ?? state.stage;
      if (exitedStage) {
        const stageState = state.stages[exitedStage];
        const enteredAt = stageState.enteredAtMs ?? timestampMs;
        const explicitDuration = toNumber(payload.duration_ms, -1);
        const duration = explicitDuration >= 0 ? explicitDuration : Math.max(0, timestampMs - enteredAt);
        state.stages = {
          ...state.stages,
          [exitedStage]: {
            ...stageState,
            exitedAtMs: timestampMs,
            durationMs: duration,
          },
        };
      }
      break;
    }
    case 'stage.transition': {
      const from = String(payload.from ?? base.stage ?? '');
      const to = String(payload.to ?? event.stage ?? '');
      state.transitions = [...base.transitions, { from, to, seq }];
      const nextStage = normalizeStage(to);
      if (nextStage) {
        state.stage = nextStage;
      }
      break;
    }
    case 'stage.illegal_transition': {
      state.errorCount += 1;
      const message = `非法阶段转移 ${String(payload.from ?? '?')}→${String(payload.to ?? '?')}`;
      state.anomalies = pushAnomaly(state, event, message);
      state.lastError = state.anomalies[0];
      break;
    }
    case 'invalid_finish': {
      state.errorCount += 1;
      state.anomalies = pushAnomaly(state, event, String(payload.reason ?? '阶段完成载荷非法'));
      state.lastError = state.anomalies[0];
      break;
    }
    case 'acceptance.started':
      state.status = isTerminalStatus(state.status) ? state.status : 'waiting_acceptance';
      break;
    case 'acceptance.passed':
      state.acceptanceTotal += 1;
      state.acceptancePassed += 1;
      state.recentVerdict = 'passed';
      if (!isTerminalStatus(state.status)) {
        state.status = 'running';
      }
      break;
    case 'acceptance.failed':
      state.acceptanceTotal += 1;
      state.acceptanceFailed += 1;
      state.recentVerdict = 'failed';
      if (!isTerminalStatus(state.status)) {
        state.status = 'running';
      }
      break;
    case 'acceptance.invalid':
      state.acceptanceTotal += 1;
      state.recentVerdict = 'invalid';
      state.errorCount += 1;
      state.anomalies = pushAnomaly(state, event, String(payload.reason ?? '验收记录非法'));
      state.lastError = state.anomalies[0];
      break;
    case 'acceptance.rollback_overridden':
      state.anomalies = pushAnomaly(state, event, String(payload.reason ?? '回退目标被覆盖'));
      break;
    case 'retry.scheduled':
      state.retryCount += 1;
      if (!isTerminalStatus(state.status)) {
        state.status = 'retrying';
      }
      break;
    case 'retry.exhausted':
      state.status = 'failed';
      state.terminalReason = String(payload.reason ?? 'retry_exhausted');
      break;
    case 'checkpoint.saved':
    case 'checkpoint.restored':
      state.lastCheckpointId = String(payload.checkpoint_id ?? base.lastCheckpointId);
      break;
    case 'checkpoint.drift':
      state.anomalies = pushAnomaly(state, event, String(payload.reason ?? '检查点漂移'));
      break;
    case 'checkpoint.corrupted':
      state.errorCount += 1;
      state.anomalies = pushAnomaly(state, event, String(payload.reason ?? '检查点损坏'));
      state.lastError = state.anomalies[0];
      break;
    case 'compaction.triggered':
      state.compactionCount += 1;
      break;
    case 'compaction.completed':
      state.compactionCount += 1;
      break;
    case 'compaction.degraded':
      state.degradedCount += 1;
      state.anomalies = pushAnomaly(state, event, String(payload.reason ?? '上下文压缩降级'));
      break;
    case 'compaction.rejected':
      state.degradedCount += 1;
      state.anomalies = pushAnomaly(state, event, String(payload.reason ?? '上下文压缩被拒绝'));
      break;
    case 'memory.written':
    case 'memory.merged':
    case 'memory.promoted':
    case 'memory.evicted':
    case 'memory.decayed':
    case 'memory.resync':
      state.memoryEventCount += 1;
      break;
    case 'memory.corrupted':
      state.memoryEventCount += 1;
      state.errorCount += 1;
      state.anomalies = pushAnomaly(state, event, String(payload.reason ?? '记忆文件损坏'));
      state.lastError = state.anomalies[0];
      break;
    case 'memory.index_degraded':
      state.memoryEventCount += 1;
      state.anomalies = pushAnomaly(state, event, String(payload.reason ?? '记忆索引降级'));
      break;
    case 'index.queued':
      state.indexStatus = 'queued';
      break;
    case 'index.updated':
      state.indexStatus = 'updated';
      break;
    case 'index.rebuilt':
      state.indexStatus = 'rebuilt';
      break;
    case 'index.failed':
      state.indexStatus = 'failed';
      state.errorCount += 1;
      state.anomalies = pushAnomaly(state, event, String(payload.reason ?? '索引失败'));
      state.lastError = state.anomalies[0];
      break;
    case 'tool.started':
    case 'tool.result':
      state.toolCalls += 1;
      break;
    case 'tool.progress':
      break;
    case 'tool.denied':
      state.securityViolations += 1;
      state.errorCount += 1;
      state.anomalies = pushAnomaly(state, event, String(payload.reason ?? payload.code ?? '工具调用被策略拒绝'));
      state.lastError = state.anomalies[0];
      break;
    case 'subtask.plan_created':
      if (!isTerminalStatus(state.status)) {
        state.status = 'running';
      }
      break;
    case 'subtask.queued':
    case 'subtask.started':
    case 'subtask.handoff_waiting':
    case 'subtask.accepted':
      break;
    case 'subtask.rejected':
    case 'subtask.failed':
    case 'subtask.cancelled':
      state.anomalies = pushAnomaly(
        state,
        event,
        String(payload.reason ?? payload.category ?? '子任务未完成'),
      );
      break;
    case 'subtask.circuit_open':
      state.errorCount += 1;
      state.anomalies = pushAnomaly(
        state,
        event,
        `子任务熔断 ${String(payload.class_key ?? '')}`.trim(),
      );
      state.lastError = state.anomalies[0];
      break;
    case 'handoff.created':
    case 'handoff.accepted':
      break;
    case 'handoff.rejected':
      state.anomalies = pushAnomaly(state, event, String(payload.reason ?? '交接被拒绝'));
      break;
    case 'aggregation.completed':
      if (payload.complete === false) {
        state.anomalies = pushAnomaly(state, event, '父级汇聚未完成');
      }
      break;
    case 'aggregation.conflict':
      state.errorCount += 1;
      state.anomalies = pushAnomaly(
        state,
        event,
        `汇聚冲突 ${String(payload.kind ?? '')} ${String(payload.path ?? '')}`.trim(),
      );
      state.lastError = state.anomalies[0];
      break;
    case 'model.selected':
    case 'model.switched':
    case 'model.fallback':
    case 'model.recovered':
    case 'model.registry_changed':
    case 'model.config_applied':
      break;
    case 'model.capability_rejected':
      state.anomalies = pushAnomaly(
        state,
        event,
        String(payload.reason ?? payload.capability ?? '模型能力不满足'),
      );
      break;
    case 'model.unavailable':
      state.anomalies = pushAnomaly(
        state,
        event,
        `模型不可用 ${String(payload.model_id ?? '')}`.trim(),
      );
      break;
    case 'approval.requested':
    case 'approval.approved':
      break;
    case 'approval.rejected':
    case 'approval.timeout':
    case 'approval.cancelled':
      state.anomalies = pushAnomaly(
        state,
        event,
        String(payload.reason ?? payload.code ?? '审批未通过'),
      );
      break;
    case 'error':
      state.errorCount += 1;
      state.anomalies = pushAnomaly(
        state,
        event,
        String(payload.reason ?? payload.message ?? '执行错误'),
      );
      state.lastError = state.anomalies[0];
      break;
    case 'security.violation':
      state.securityViolations += 1;
      state.errorCount += 1;
      state.anomalies = pushAnomaly(state, event, String(payload.reason ?? '安全约束拒绝'));
      state.lastError = state.anomalies[0];
      break;
    case 'state.rebuild':
      state.anomalies = pushAnomaly(state, event, String(payload.reason ?? '状态重建'));
      break;
    case 'event.invalid':
      state.invalidEvents += 1;
      state.anomalies = pushAnomaly(state, event, String(payload.reason ?? '非法事件'));
      break;
    default:
      state.invalidEvents += 1;
      break;
  }

  state.healthScore = computeHealth(state).score;
  return state;
}

export function reduceTaskEvents(
  previous: TaskRuntimeState | undefined,
  events: HarnessEvent[],
): TaskRuntimeState {
  const ordered = [...events].sort((left, right) => toNumber(left.seq) - toNumber(right.seq));
  return ordered.reduce<TaskRuntimeState | undefined>(
    (state, event) => reduceTaskState(state, event),
    previous,
  ) ?? createTaskState(events[0]?.task_id ?? '');
}

export function reduceMonitorState(
  previous: Record<string, TaskRuntimeState>,
  events: HarnessEvent[],
): Record<string, TaskRuntimeState> {
  if (events.length === 0) {
    return previous;
  }
  const grouped = new Map<string, HarnessEvent[]>();
  for (const event of events) {
    const taskId = event.task_id;
    if (!taskId) {
      continue;
    }
    const bucket = grouped.get(taskId);
    if (bucket) {
      bucket.push(event);
    } else {
      grouped.set(taskId, [event]);
    }
  }
  let next = previous;
  for (const [taskId, taskEvents] of grouped) {
    next = { ...next, [taskId]: reduceTaskEvents(next[taskId], taskEvents) };
  }
  return next;
}

export function hydrateTaskState(view: HarnessTaskView): TaskRuntimeState {
  const stage = normalizeStage(view.current_stage);
  const attemptNoByStage = { a: 0, b: 0, c: 0, d: 0, e: 0 };
  for (const code of STAGES) {
    attemptNoByStage[code] = toNumber(view.attempt_no_by_stage?.[code]);
  }
  const status = (view.status as TaskStatus) || 'pending';
  return createTaskState(view.task_id, {
    tenantId: view.tenant_id ?? '',
    projectId: view.project_id ?? '',
    goal: view.goal ?? '',
    stage,
    status,
    terminalReason: view.terminal_reason ?? '',
    attemptNoByStage,
    retryCount: toNumber(view.retry_count),
    acceptanceFailed: toNumber(view.acceptance_fail_count),
    startedAtMs: toNumber(view.created_at_ms) || null,
    updatedAtMs: toNumber(view.updated_at_ms),
    lastCheckpointId: view.last_checkpoint_id ?? '',
    stages: {
      ...createEmptyStageMap(),
      ...(stage
        ? {
            [stage]: {
              ...createEmptyStageRuntime(),
              lastAttempt: attemptNoByStage[stage],
              attempts: attemptNoByStage[stage],
              enteredAtMs: toNumber(view.updated_at_ms) || null,
            },
          }
        : {}),
    } as Record<StageCode, StageRuntime>,
  });
}

export function statusLabel(status: TaskStatus): string {
  const labels: Record<TaskStatus, string> = {
    pending: '待启动',
    running: '运行中',
    waiting_acceptance: '等待验收',
    retrying: '重试中',
    paused: '已暂停',
    completed: '完成',
    failed: '失败',
    aborted: '已中止',
  };
  return labels[status] ?? status;
}

export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) {
    return '—';
  }
  if (ms < 1000) {
    return `${Math.round(ms)}ms`;
  }
  const seconds = Math.floor(ms / 1000);
  if (seconds < 60) {
    return `${seconds}s`;
  }
  const minutes = Math.floor(seconds / 60);
  const remain = seconds % 60;
  if (minutes < 60) {
    return `${minutes}m${remain}s`;
  }
  const hours = Math.floor(minutes / 60);
  return `${hours}h${minutes % 60}m`;
}