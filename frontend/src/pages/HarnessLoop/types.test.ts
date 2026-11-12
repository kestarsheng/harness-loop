import { describe, expect, it } from 'vitest';
import type { HarnessEvent, TaskRuntimeState } from './types';
import {
  KNOWN_EVENT_TYPES,
  computeHealth,
  createTaskState,
  healthLevel,
  hydrateTaskState,
  parsePayload,
  reduceMonitorState,
  reduceTaskEvents,
  reduceTaskState,
  toNumber,
} from './types';

function makeEvent(
  type: string,
  overrides: Partial<HarnessEvent> = {},
  seq = 1,
): HarnessEvent {
  return {
    event_id: `event-${type}-${seq}`,
    event_type: type,
    task_id: 'task-1',
    tenant_id: 'tenant-a',
    project_id: 'proj-a',
    seq,
    timestamp_ms: 1_000 + seq * 100,
    severity: 'info',
    payload: '{}',
    ...overrides,
  };
}

interface EventCase {
  type: string;
  payload?: Record<string, unknown>;
  stage?: string;
  attempt?: number;
  prepare?: (state: TaskRuntimeState | undefined) => TaskRuntimeState;
  assert?: (state: TaskRuntimeState) => void;
}

const CASES: EventCase[] = [
  {
    type: 'task.started',
    payload: { goal: '补全登录模块单测' },
    assert: (state) => {
      expect(state.status).toBe('running');
      expect(state.goal).toBe('补全登录模块单测');
      expect(state.startedAtMs).not.toBeNull();
    },
  },
  { type: 'task.completed', payload: { terminal_reason: 'completed' }, assert: (s) => expect(s.status).toBe('completed') },
  { type: 'task.aborted', payload: { terminal_reason: 'aborted' }, assert: (s) => expect(s.status).toBe('aborted') },
  { type: 'task.failed', payload: { terminal_reason: 'failed' }, assert: (s) => { expect(s.status).toBe('failed'); expect(s.lastError).not.toBeNull(); } },
  { type: 'task.paused', assert: (s) => expect(s.status).toBe('paused') },
  {
    type: 'task.resumed',
    prepare: (state) => reduceTaskState(state, makeEvent('task.paused')),
    assert: (s) => expect(s.status).toBe('running'),
  },
  {
    type: 'stage.entered',
    stage: 'c',
    attempt: 2,
    assert: (s) => {
      expect(s.stage).toBe('c');
      expect(s.attemptNoByStage.c).toBe(2);
      expect(s.attempts).toBe(1);
      expect(s.stages.c.attempts).toBe(1);
    },
  },
  {
    type: 'stage.exited',
    stage: 'c',
    payload: { duration_ms: 1500 },
    prepare: (state) =>
      reduceTaskState(state, makeEvent('stage.entered', { stage: 'c', attempt: 1 })),
    assert: (s) => {
      expect(s.stages.c.durationMs).toBe(1500);
      expect(s.stages.c.exitedAtMs).not.toBeNull();
    },
  },
  {
    type: 'stage.transition',
    stage: 'b',
    payload: { from: 'a', to: 'b' },
    assert: (s) => {
      expect(s.transitions).toHaveLength(1);
      expect(s.stage).toBe('b');
    },
  },
  {
    type: 'stage.illegal_transition',
    payload: { from: 'c', to: 'a' },
    assert: (s) => {
      expect(s.errorCount).toBe(1);
      expect(s.anomalies).toHaveLength(1);
    },
  },
  {
    type: 'invalid_finish',
    payload: { reason: 'finish payload missing stage' },
    assert: (s) => {
      expect(s.errorCount).toBe(1);
      expect(s.lastError?.message).toContain('finish');
    },
  },
  { type: 'acceptance.started', assert: (s) => expect(s.status).toBe('waiting_acceptance') },
  {
    type: 'acceptance.passed',
    assert: (s) => {
      expect(s.acceptanceTotal).toBe(1);
      expect(s.acceptancePassed).toBe(1);
      expect(s.recentVerdict).toBe('passed');
    },
  },
  {
    type: 'acceptance.failed',
    assert: (s) => {
      expect(s.acceptanceTotal).toBe(1);
      expect(s.acceptanceFailed).toBe(1);
      expect(s.recentVerdict).toBe('failed');
    },
  },
  {
    type: 'acceptance.invalid',
    payload: { reason: 'verdict missing' },
    assert: (s) => {
      expect(s.recentVerdict).toBe('invalid');
      expect(s.errorCount).toBe(1);
    },
  },
  {
    type: 'acceptance.rollback_overridden',
    payload: { reason: 'fallback target overwritten' },
    assert: (s) => expect(s.anomalies).toHaveLength(1),
  },
  {
    type: 'retry.scheduled',
    assert: (s) => {
      expect(s.retryCount).toBe(1);
      expect(s.status).toBe('retrying');
    },
  },
  {
    type: 'retry.exhausted',
    payload: { reason: 'max_retry' },
    assert: (s) => {
      expect(s.status).toBe('failed');
      expect(s.terminalReason).toBe('max_retry');
    },
  },
  {
    type: 'checkpoint.saved',
    payload: { checkpoint_id: 'ckpt-1' },
    assert: (s) => expect(s.lastCheckpointId).toBe('ckpt-1'),
  },
  {
    type: 'checkpoint.restored',
    payload: { checkpoint_id: 'ckpt-2' },
    assert: (s) => expect(s.lastCheckpointId).toBe('ckpt-2'),
  },
  {
    type: 'checkpoint.drift',
    payload: { reason: 'workspace changed' },
    assert: (s) => expect(s.anomalies).toHaveLength(1),
  },
  {
    type: 'checkpoint.corrupted',
    payload: { reason: 'checksum mismatch' },
    assert: (s) => {
      expect(s.errorCount).toBe(1);
      expect(s.anomalies).toHaveLength(1);
    },
  },
  {
    type: 'compaction.triggered',
    assert: (s) => expect(s.compactionCount).toBe(1),
  },
  {
    type: 'compaction.completed',
    assert: (s) => expect(s.compactionCount).toBe(1),
  },
  {
    type: 'compaction.degraded',
    payload: { reason: 'level 1 truncation' },
    assert: (s) => {
      expect(s.degradedCount).toBe(1);
      expect(s.anomalies).toHaveLength(1);
    },
  },
  {
    type: 'compaction.rejected',
    payload: { reason: 'summary incomplete' },
    assert: (s) => {
      expect(s.degradedCount).toBe(1);
      expect(s.anomalies).toHaveLength(1);
    },
  },
  ...['memory.written', 'memory.merged', 'memory.promoted', 'memory.evicted', 'memory.decayed', 'memory.resync'].map(
    (type): EventCase => ({
      type,
      assert: (s) => expect(s.memoryEventCount).toBe(1),
    }),
  ),
  {
    type: 'memory.corrupted',
    payload: { reason: 'invalid markdown' },
    assert: (s) => {
      expect(s.memoryEventCount).toBe(1);
      expect(s.errorCount).toBe(1);
      expect(s.anomalies).toHaveLength(1);
    },
  },
  {
    type: 'memory.index_degraded',
    payload: { reason: 'fallback keyword recall' },
    assert: (s) => {
      expect(s.memoryEventCount).toBe(1);
      expect(s.anomalies).toHaveLength(1);
    },
  },
  { type: 'index.queued', assert: (s) => expect(s.indexStatus).toBe('queued') },
  { type: 'index.updated', assert: (s) => expect(s.indexStatus).toBe('updated') },
  { type: 'index.rebuilt', assert: (s) => expect(s.indexStatus).toBe('rebuilt') },
  {
    type: 'index.failed',
    payload: { reason: 'embedding unavailable' },
    assert: (s) => {
      expect(s.indexStatus).toBe('failed');
      expect(s.errorCount).toBe(1);
    },
  },
  { type: 'tool.started', assert: (s) => expect(s.toolCalls).toBe(1) },
  { type: 'tool.result', assert: (s) => expect(s.toolCalls).toBe(1) },
  { type: 'tool.progress', assert: (s) => expect(s.eventCount).toBe(1) },
  {
    type: 'tool.denied',
    payload: { tool: 'write_file', code: 'E_TOOL_DENIED' },
    assert: (s) => {
      expect(s.securityViolations).toBe(1);
      expect(s.errorCount).toBe(1);
      expect(s.anomalies).toHaveLength(1);
    },
  },
  {
    type: 'subtask.plan_created',
    payload: { dag_id: 'dag-1', node_count: 3 },
    assert: (s) => {
      expect(s.status).toBe('running');
      expect(s.anomalies).toHaveLength(0);
    },
  },
  { type: 'subtask.queued', payload: { position: 1 }, assert: (s) => expect(s.anomalies).toHaveLength(0) },
  { type: 'subtask.started', payload: { role: 'backend' }, assert: (s) => expect(s.anomalies).toHaveLength(0) },
  { type: 'subtask.handoff_waiting', assert: (s) => expect(s.anomalies).toHaveLength(0) },
  { type: 'subtask.accepted', payload: { acceptance_id: 'acc-1' }, assert: (s) => expect(s.anomalies).toHaveLength(0) },
  {
    type: 'subtask.rejected',
    payload: { reason: 'self verification failed' },
    assert: (s) => expect(s.anomalies).toHaveLength(1),
  },
  {
    type: 'subtask.failed',
    payload: { category: 'timeout' },
    assert: (s) => expect(s.anomalies).toHaveLength(1),
  },
  {
    type: 'subtask.cancelled',
    payload: { reason: 'parent replanned' },
    assert: (s) => expect(s.anomalies).toHaveLength(1),
  },
  {
    type: 'subtask.circuit_open',
    payload: { class_key: 'backend/code', failure_rate: 0.8 },
    assert: (s) => {
      expect(s.errorCount).toBe(1);
      expect(s.anomalies).toHaveLength(1);
    },
  },
  { type: 'handoff.created', payload: { from_agent_id: 'a1', to_agent_id: 'a2' }, assert: (s) => expect(s.anomalies).toHaveLength(0) },
  { type: 'handoff.accepted', assert: (s) => expect(s.anomalies).toHaveLength(0) },
  { type: 'handoff.rejected', payload: { reason: 'contract mismatch' }, assert: (s) => expect(s.anomalies).toHaveLength(1) },
  {
    type: 'aggregation.completed',
    payload: { total: 3, accepted: 2, complete: false },
    assert: (s) => expect(s.anomalies).toHaveLength(1),
  },
  {
    type: 'aggregation.conflict',
    payload: { kind: 'file_overlap', path: 'src/a.ts' },
    assert: (s) => {
      expect(s.errorCount).toBe(1);
      expect(s.anomalies).toHaveLength(1);
    },
  },
  { type: 'model.selected', payload: { model_id: 'qwen-max', stage: 'c' }, assert: (s) => expect(s.anomalies).toHaveLength(0) },
  { type: 'model.switched', payload: { from: 'qwen-plus', to: 'qwen-max' }, assert: (s) => expect(s.anomalies).toHaveLength(0) },
  { type: 'model.fallback', payload: { primary: 'qwen-plus', fallback: 'qwen-max' }, assert: (s) => expect(s.anomalies).toHaveLength(0) },
  {
    type: 'model.capability_rejected',
    payload: { capability: 'vision' },
    assert: (s) => expect(s.anomalies).toHaveLength(1),
  },
  { type: 'model.recovered', payload: { model_id: 'qwen-plus' }, assert: (s) => expect(s.anomalies).toHaveLength(0) },
  {
    type: 'model.unavailable',
    payload: { model_id: 'qwen-turbo', fail_count: 3 },
    assert: (s) => expect(s.anomalies).toHaveLength(1),
  },
  { type: 'model.registry_changed', payload: { version: 'v2', result: 'applied' }, assert: (s) => expect(s.anomalies).toHaveLength(0) },
  { type: 'model.config_applied', payload: { version: 'v2' }, assert: (s) => expect(s.anomalies).toHaveLength(0) },
  { type: 'approval.requested', payload: { tool: 'git_push', code: 'approval_required' }, assert: (s) => expect(s.anomalies).toHaveLength(0) },
  { type: 'approval.approved', assert: (s) => expect(s.anomalies).toHaveLength(0) },
  { type: 'approval.rejected', payload: { code: 'E_APPROVAL_REJECTED' }, assert: (s) => expect(s.anomalies).toHaveLength(1) },
  { type: 'approval.timeout', payload: { code: 'E_APPROVAL_TIMEOUT' }, assert: (s) => expect(s.anomalies).toHaveLength(1) },
  { type: 'approval.cancelled', payload: { code: 'E_APPROVAL_CANCELLED' }, assert: (s) => expect(s.anomalies).toHaveLength(1) },
  {
    type: 'error',
    payload: { reason: 'agent timeout' },
    assert: (s) => {
      expect(s.errorCount).toBe(1);
      expect(s.lastError?.message).toBe('agent timeout');
    },
  },
  {
    type: 'security.violation',
    payload: { reason: 'path escape denied' },
    assert: (s) => {
      expect(s.securityViolations).toBe(1);
      expect(s.errorCount).toBe(1);
    },
  },
  {
    type: 'state.rebuild',
    payload: { reason: 'event replay' },
    assert: (s) => expect(s.anomalies).toHaveLength(1),
  },
  {
    type: 'event.invalid',
    payload: { reason: 'schema mismatch' },
    assert: (s) => expect(s.invalidEvents).toBe(1),
  },
];

describe('reduceTaskState event coverage', () => {
  it('covers every known event type exactly once', () => {
    const covered = new Set(CASES.map((item) => item.type));
    expect(covered.size).toBe(CASES.length);
    for (const type of KNOWN_EVENT_TYPES) {
      expect(covered.has(type)).toBe(true);
    }
    expect(KNOWN_EVENT_TYPES).toHaveLength(73);

  });

  it.each(CASES)('reduces $type', (item) => {
    const base = item.prepare ? item.prepare(undefined) : undefined;
    const nextSeq = (base?.lastEventSeq ?? 0) + 1;
    const event = makeEvent(
      item.type,
      {
        stage: item.stage,
        attempt: item.attempt,
        payload: JSON.stringify(item.payload ?? {}),
      },
      nextSeq,
    );
    const state = reduceTaskState(base, event);

    expect(state.eventCount).toBe((base?.eventCount ?? 0) + 1);
    expect(state.lastEventSeq).toBe(nextSeq);
    expect(state.eventIds[event.event_id]).toBe(nextSeq);
    expect(state.recentEvents.at(-1)?.event_id).toBe(event.event_id);
    expect(state.healthScore).toBeGreaterThanOrEqual(0);
    expect(state.healthScore).toBeLessThanOrEqual(100);
    item.assert?.(state);
  });
});

describe('reduceTaskState ordering and idempotency', () => {
  it('ignores duplicate event_id and keeps the same reference', () => {
    const event = makeEvent('task.started');
    const first = reduceTaskState(undefined, event);
    const second = reduceTaskState(first, event);
    expect(second).toBe(first);
    expect(second.eventCount).toBe(1);
  });

  it('ignores stale seq to prevent state rollback', () => {
    const started = reduceTaskState(undefined, makeEvent('task.started', {}, 5));
    const stale = reduceTaskState(started, makeEvent('task.failed', {}, 3));
    expect(stale).toBe(started);
    expect(stale.status).toBe('running');
  });

  it('sorts out-of-order events by seq before reducing', () => {
    const state = reduceTaskEvents(undefined, [
      makeEvent('task.completed', { payload: '{"terminal_reason":"completed"}' }, 3),
      makeEvent('task.started', {}, 2),
      makeEvent('stage.entered', { stage: 'a', attempt: 1 }, 1),
    ]);
    expect(state.status).toBe('completed');
    expect(state.lastEventSeq).toBe(3);
    expect(state.stage).toBe('a');
  });

  it('groups monitor events by task id', () => {
    const next = reduceMonitorState({}, [
      makeEvent('task.started', { event_id: 'e1', task_id: 't1' }, 1),
      makeEvent('task.started', { event_id: 'e2', task_id: 't2' }, 1),
      makeEvent('task.completed', { event_id: 'e3', task_id: 't1' }, 2),
    ]);
    expect(Object.keys(next).sort()).toEqual(['t1', 't2']);
    expect(next.t1.status).toBe('completed');
    expect(next.t2.status).toBe('running');
  });
});

describe('health calculation', () => {
  it('marks cold start as insufficient sample with neutral acceptance rate', () => {
    const health = computeHealth(createTaskState('task-1'));
    expect(health.sampleInsufficient).toBe(true);
    expect(health.acceptancePassRate).toBe(0.5);
  });

  it('scores a high-retry low-pass task lower than a healthy task', () => {
    let unhealthy = createTaskState('bad');
    unhealthy = reduceTaskState(unhealthy, makeEvent('task.started', { event_id: 'b1' }, 1));
    unhealthy = reduceTaskState(unhealthy, makeEvent('stage.entered', { event_id: 'b2', stage: 'c', attempt: 1 }, 2));
    for (let index = 0; index < 4; index += 1) {
      unhealthy = reduceTaskState(
        unhealthy,
        makeEvent('retry.scheduled', { event_id: `b-retry-${index}` }, 3 + index),
      );
    }
    unhealthy = reduceTaskState(
      unhealthy,
      makeEvent('acceptance.failed', { event_id: 'b-acc' }, 10),
    );
    unhealthy = reduceTaskState(
      unhealthy,
      makeEvent('acceptance.failed', { event_id: 'b-acc-2' }, 11),
    );
    unhealthy = reduceTaskState(unhealthy, makeEvent('error', { event_id: 'b-err' }, 12));

    let healthy = createTaskState('good');
    healthy = reduceTaskState(healthy, makeEvent('task.started', { event_id: 'g1' }, 1));
    healthy = reduceTaskState(healthy, makeEvent('stage.entered', { event_id: 'g2', stage: 'c', attempt: 1 }, 2));
    healthy = reduceTaskState(healthy, makeEvent('acceptance.passed', { event_id: 'g3' }, 3));
    healthy = reduceTaskState(healthy, makeEvent('acceptance.passed', { event_id: 'g4' }, 4));

    expect(unhealthy.healthScore).toBeLessThan(healthy.healthScore);
    expect(healthLevel(unhealthy.healthScore)).toBe('danger');
    expect(healthLevel(healthy.healthScore)).toBe('ok');
  });

  it('keeps freshness at 1 for terminal tasks', () => {
    let state = createTaskState('task-1');
    state = reduceTaskState(state, makeEvent('task.completed', { event_id: 'c1' }, 1));
    const health = computeHealth(state, Date.now() + 3_600_000, 60_000);
    expect(health.stageFreshness).toBe(1);
  });
});

describe('payload and view helpers', () => {
  it('parses JSON string payload', () => {
    expect(parsePayload('{"reason":"x"}')).toEqual({ reason: 'x' });
  });

  it('parses base64 encoded payload', () => {
    const encoded = btoa('{"reason":"encoded"}');
    expect(parsePayload(encoded)).toEqual({ reason: 'encoded' });
  });

  it('returns empty object for broken payload', () => {
    expect(parsePayload('not-json')).toEqual({});
    expect(parsePayload(undefined)).toEqual({});
  });

  it('normalizes numeric strings from protojson', () => {
    expect(toNumber('42')).toBe(42);
    expect(toNumber(undefined, 7)).toBe(7);
    expect(toNumber('abc', 3)).toBe(3);
  });

  it('hydrates task view without inventing acceptance samples', () => {
    const state = hydrateTaskState({
      task_id: 'task-9',
      project_id: 'proj-a',
      current_stage: 'c',
      status: 'running',
      attempt_no_by_stage: { a: 1, b: 1, c: 2 },
      retry_count: 1,
      acceptance_fail_count: 1,
      created_at_ms: 1000,
      updated_at_ms: 5000,
      last_checkpoint_id: 'ckpt-9',
    });
    expect(state.stage).toBe('c');
    expect(state.attemptNoByStage.c).toBe(2);
    expect(state.lastCheckpointId).toBe('ckpt-9');
    expect(state.acceptanceTotal).toBe(0);
  });
});