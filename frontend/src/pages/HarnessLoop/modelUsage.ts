import type { HarnessEvent, StageCode } from './types';
import { normalizeStage, parsePayload, toNumber } from './types';

export const MODEL_USAGE_EVENT_TYPES = [
  'model.selected',
  'model.switched',
  'model.fallback',
  'model.capability_rejected',
  'model.recovered',
  'model.unavailable',
  'model.registry_changed',
  'model.config_applied',
] as const;

export type ModelUsageEventType = (typeof MODEL_USAGE_EVENT_TYPES)[number];

export const FALLBACK_TRIGGERS = [
  'timeout',
  'rate_limit',
  'error',
  'budget',
  'manual',
  'unknown',
] as const;

export type ModelFallbackTrigger = (typeof FALLBACK_TRIGGERS)[number];

export interface ModelStageUsage {
  stage: StageCode;
  model: string;
  selections: number;
  fallbackCount: number;
  tokens: number;
  cost: number;
  lastSelectedAtMs: number;
}

export interface ModelFallbackPoint {
  subtaskOrStage: string;
  stage: StageCode | null;
  fromModel: string;
  toModel: string;
  trigger: ModelFallbackTrigger;
  timestampMs: number;
}

export interface ModelUsageState {
  stages: Record<StageCode, ModelStageUsage>;
  modelCalls: Record<string, number>;
  fallbackPoints: ModelFallbackPoint[];
  configVersion: string;
  lastEventSeq: number;
  seenEventIds: Record<string, number>;
}

export interface ModelDistributionEntry {
  model: string;
  calls: number;
}

export interface FallbackTrend {
  bucketStartMs: number[];
  triggers: ModelFallbackTrigger[];
  series: Array<{ trigger: ModelFallbackTrigger; values: number[] }>;
}

const STAGE_LIST: StageCode[] = ['a', 'b', 'c', 'd', 'e'];

function payloadString(payload: Record<string, unknown>, keys: string[]): string {
  for (const key of keys) {
    const value = payload[key];
    if (typeof value === 'string' && value.trim() !== '') {
      return value.trim();
    }
  }
  return '';
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

export function createModelUsageState(): ModelUsageState {
  return {
    stages: {
      a: emptyStageUsage('a'),
      b: emptyStageUsage('b'),
      c: emptyStageUsage('c'),
      d: emptyStageUsage('d'),
      e: emptyStageUsage('e'),
    },
    modelCalls: {},
    fallbackPoints: [],
    configVersion: '',
    lastEventSeq: 0,
    seenEventIds: {},
  };
}

export function normalizeFallbackTrigger(value: unknown): ModelFallbackTrigger {
  const text = typeof value === 'string' ? value.trim().toLowerCase() : '';
  return (FALLBACK_TRIGGERS as readonly string[]).includes(text)
    ? (text as ModelFallbackTrigger)
    : 'unknown';
}

export function reduceModelUsage(
  previous: ModelUsageState | undefined,
  event: HarnessEvent,
): ModelUsageState {
  const base = previous ?? createModelUsageState();

  if (!event.event_id) {
    return base;
  }
  if (Object.prototype.hasOwnProperty.call(base.seenEventIds, event.event_id)) {
    return base;
  }
  if (!(MODEL_USAGE_EVENT_TYPES as readonly string[]).includes(event.event_type)) {
    return base;
  }

  const seq = toNumber(event.seq);
  if (previous && seq <= previous.lastEventSeq) {
    return previous;
  }

  const payload = parsePayload(event.payload);
  const stage = normalizeStage(event.stage ?? payload.stage ?? payload.to_stage);
  const tokensDelta = toNumber(payload.tokens ?? payload.token_delta);
  const costDelta = toNumber(payload.cost ?? payload.cost_delta);
  const timestampMs = toNumber(event.timestamp_ms);

  const modelCalls = { ...base.modelCalls };
  const stages = { ...base.stages };
  const stageUsage = stage ? { ...stages[stage] } : null;
  const fallbackPoints = base.fallbackPoints;
  let configVersion = base.configVersion;

  switch (event.event_type) {
    case 'model.selected': {
      const model =
        payloadString(payload, ['model', 'model_id', 'model_name', 'effective_model']) ||
        'unknown';
      modelCalls[model] = (modelCalls[model] ?? 0) + 1;
      if (stageUsage) {
        stageUsage.model = model;
        stageUsage.selections += 1;
        stageUsage.tokens += tokensDelta;
        stageUsage.cost += costDelta;
        stageUsage.lastSelectedAtMs = timestampMs;
      }
      break;
    }
    case 'model.fallback': {
      const fromModel = payloadString(payload, ['from', 'from_model', 'previous_model']);
      const toModel = payloadString(payload, ['to', 'to_model', 'effective_model']);
      const trigger = normalizeFallbackTrigger(
        payload.trigger ?? payload.reason ?? payload.fallback_reason,
      );
      if (stageUsage) {
        stageUsage.fallbackCount += 1;
        if (toModel) {
          stageUsage.model = toModel;
        }
        stageUsage.tokens += tokensDelta;
        stageUsage.cost += costDelta;
      }
      fallbackPoints.push({
        subtaskOrStage: payloadString(payload, ['subtask_id', 'stage_name']) || stage || '',
        stage,
        fromModel,
        toModel,
        trigger,
        timestampMs,
      });
      break;
    }
    case 'model.switched': {
      const toModel =
        payloadString(payload, ['to', 'to_model', 'effective_model', 'model_id']);
      if (stageUsage && toModel) {
        stageUsage.model = toModel;
        stageUsage.lastSelectedAtMs = timestampMs;
      }
      break;
    }
    case 'model.capability_rejected':
    case 'model.unavailable':
      break;
    case 'model.registry_changed': {
      const version = payloadString(payload, ['version', 'config_version']);
      if (version) {
        configVersion = version;
      }
      break;
    }
    case 'model.recovered': {
      const model =
        payloadString(payload, ['model', 'model_id', 'model_name', 'effective_model', 'to']);
      if (stageUsage && model) {
        stageUsage.model = model;
        stageUsage.tokens += tokensDelta;
        stageUsage.cost += costDelta;
      }
      break;
    }
    case 'model.config_applied': {
      configVersion =
        payloadString(payload, ['version', 'config_version', 'revision']) || configVersion;
      break;
    }
    default:
      break;
  }

  if (stage && stageUsage) {
    stages[stage] = stageUsage;
  }

  return {
    stages,
    modelCalls,
    fallbackPoints,
    configVersion,
    lastEventSeq: Math.max(base.lastEventSeq, seq),
    seenEventIds: { ...base.seenEventIds, [event.event_id]: seq },
  };
}

export function computeModelDistribution(
  state: ModelUsageState,
  topN = 5,
): ModelDistributionEntry[] {
  return Object.entries(state.modelCalls)
    .map(([model, calls]) => ({ model, calls }))
    .filter((entry) => entry.calls > 0)
    .sort((left, right) => {
      if (right.calls !== left.calls) {
        return right.calls - left.calls;
      }
      return left.model.localeCompare(right.model);
    })
    .slice(0, Math.max(1, topN));
}

export function computeFallbackTrend(
  state: ModelUsageState,
  nowMs: number,
  windowMs = 60 * 60 * 1000,
  bucketMs = 10 * 60 * 1000,
): FallbackTrend {
  const safeBucketMs = Math.max(1, bucketMs);
  const startMs = nowMs - Math.max(safeBucketMs, windowMs);
  const bucketCount = Math.max(1, Math.ceil((nowMs - startMs) / safeBucketMs));
  const bucketStartMs = Array.from(
    { length: bucketCount },
    (_value, index) => startMs + index * safeBucketMs,
  );

  const triggers: ModelFallbackTrigger[] = [];
  const valuesByTrigger = new Map<ModelFallbackTrigger, number[]>();
  for (const point of state.fallbackPoints) {
    if (!triggers.includes(point.trigger)) {
      triggers.push(point.trigger);
    }
    const index = Math.min(
      bucketCount - 1,
      Math.max(0, Math.floor((point.timestampMs - startMs) / safeBucketMs)),
    );
    const values = valuesByTrigger.get(point.trigger) ?? new Array(bucketCount).fill(0);
    values[index] += 1;
    valuesByTrigger.set(point.trigger, values);
  }

  triggers.sort(
    (left, right) => FALLBACK_TRIGGERS.indexOf(left) - FALLBACK_TRIGGERS.indexOf(right),
  );

  return {
    bucketStartMs,
    triggers,
    series: triggers.map((trigger) => ({
      trigger,
      values: valuesByTrigger.get(trigger) ?? new Array(bucketCount).fill(0),
    })),
  };
}

export function modelStageRows(state: ModelUsageState): ModelStageUsage[] {
  return STAGE_LIST.map((stage) => state.stages[stage]);
}