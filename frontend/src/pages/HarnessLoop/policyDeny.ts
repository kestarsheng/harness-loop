import type { HarnessEvent } from './types';
import { parsePayload, toNumber } from './types';

export const POLICY_DENY_EVENT_TYPES = ['tool.denied', 'security.violation'] as const;

export type PolicyDenyEventType = (typeof POLICY_DENY_EVENT_TYPES)[number];

export const MAX_POLICY_DENY_ITEMS = 200;

export interface PolicyDenyItem {
  eventId: string;
  seq: number;
  timestampMs: number;
  tool: string;
  ruleId: string;
  layer: string;
  reason: string;
  argSummary: string;
  stage: string | null;
  source: PolicyDenyEventType;
}

export interface PolicyDenyState {
  items: PolicyDenyItem[];
  total: number;
  byRule: Record<string, number>;
  byLayer: Record<string, number>;
  lastEventSeq: number;
  seenEventIds: Record<string, number>;
}

export interface PolicyDenyDistributionEntry {
  ruleId: string;
  layer: string;
  count: number;
}

const SENSITIVE_KEY_PATTERN = /(secret|token|password|passwd|api[_-]?key|authorization|credential)/i;
const UNCLASSIFIED_RULE = 'unclassified';

export function createPolicyDenyState(): PolicyDenyState {
  return {
    items: [],
    total: 0,
    byRule: {},
    byLayer: {},
    lastEventSeq: 0,
    seenEventIds: {},
  };
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

export function summarizeParams(args: unknown, maxLength = 120): string {
  if (args === null || args === undefined) {
    return '—';
  }

  let text: string;
  if (typeof args === 'string') {
    text = args;
  } else if (typeof args === 'object') {
    try {
      text = JSON.stringify(args, (key, value) =>
        key && SENSITIVE_KEY_PATTERN.test(key) ? '***' : value,
      );
    } catch {
      text = '[unserializable]';
    }
  } else {
    text = String(args);
  }

  const normalized = text.replace(/\s+/g, ' ').trim();
  if (normalized === '') {
    return '—';
  }
  if (normalized.length <= maxLength) {
    return normalized;
  }
  return `${normalized.slice(0, maxLength)}…`;
}

export function reducePolicyDenies(
  previous: PolicyDenyState | undefined,
  event: HarnessEvent,
): PolicyDenyState {
  const base = previous ?? createPolicyDenyState();

  if (!event.event_id) {
    return base;
  }
  if (Object.prototype.hasOwnProperty.call(base.seenEventIds, event.event_id)) {
    return base;
  }
  if (!(POLICY_DENY_EVENT_TYPES as readonly string[]).includes(event.event_type)) {
    return base;
  }

  const seq = toNumber(event.seq);
  if (previous && seq <= previous.lastEventSeq) {
    return previous;
  }

  const payload = parsePayload(event.payload);
  const source = event.event_type as PolicyDenyEventType;
  const tool =
    payloadString(payload, ['tool', 'tool_name', 'action', 'capability']) || 'unknown_tool';
  const ruleId =
    payloadString(payload, ['rule_id', 'ruleId', 'policy_id', 'rule']) || UNCLASSIFIED_RULE;
  const layer =
    payloadString(payload, ['layer', 'decision_layer', 'enforcement_layer']) ||
    (source === 'security.violation' ? 'security' : 'unknown');
  const reason =
    payloadString(payload, ['reason', 'message', 'deny_reason']) || '策略拒绝';
  const argSummary = summarizeParams(
    payload.args ?? payload.arguments ?? payload.param_summary ?? payload.params,
  );

  const item: PolicyDenyItem = {
    eventId: event.event_id,
    seq,
    timestampMs: toNumber(event.timestamp_ms),
    tool,
    ruleId,
    layer,
    reason,
    argSummary,
    stage: event.stage || payloadString(payload, ['stage']) || null,
    source,
  };

  return {
    items: [item, ...base.items].slice(0, MAX_POLICY_DENY_ITEMS),
    total: base.total + 1,
    byRule: { ...base.byRule, [ruleId]: (base.byRule[ruleId] ?? 0) + 1 },
    byLayer: { ...base.byLayer, [layer]: (base.byLayer[layer] ?? 0) + 1 },
    lastEventSeq: Math.max(base.lastEventSeq, seq),
    seenEventIds: { ...base.seenEventIds, [event.event_id]: seq },
  };
}

export function policyDenyDistribution(
  state: PolicyDenyState,
): PolicyDenyDistributionEntry[] {
  return Object.entries(state.byRule)
    .map(([ruleId, count]) => {
      let layer = 'unknown';
      const item = state.items.find((candidate) => candidate.ruleId === ruleId);
      if (item) {
        layer = item.layer;
      }
      return { ruleId, layer, count };
    })
    .sort((left, right) => {
      if (right.count !== left.count) {
        return right.count - left.count;
      }
      return left.ruleId.localeCompare(right.ruleId);
    });
}