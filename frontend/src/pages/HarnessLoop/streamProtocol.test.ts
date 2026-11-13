import { describe, expect, it } from 'vitest';
import { decodeStreamFrame, mergeLastSeq } from './streamProtocol';

describe('decodeStreamFrame', () => {
  it('event: harness 解析为业务事件', () => {
    const frame = decodeStreamFrame(
      'harness',
      JSON.stringify({
        event_id: 'ev-1',
        event_type: 'task.started',
        task_id: 'task-a2',
        seq: 7,
        timestamp_ms: 1_760_000_000_000,
        payload: '{}',
      }),
    );

    expect(frame.kind).toBe('event');
    if (frame.kind === 'event') {
      expect(frame.event.event_id).toBe('ev-1');
      expect(frame.event.seq).toBe(7);
    }
  });

  it('无 event 名的帧按业务事件兼容处理', () => {
    const frame = decodeStreamFrame(
      '',
      JSON.stringify({ event_id: 'ev-2', event_type: 'task.completed', task_id: 'task-a2', seq: 1 }),
    );
    expect(frame.kind).toBe('event');
  });

  it('event: synced 解析 last_seq', () => {
    const frame = decodeStreamFrame('synced', JSON.stringify({ last_seq: 42 }));
    expect(frame).toEqual({ kind: 'synced', lastSeq: 42 });
  });

  it('event: heartbeat 不产生业务事件', () => {
    expect(decodeStreamFrame('heartbeat', JSON.stringify({ ts: '2026-11-13T00:00:00Z' }))).toEqual({
      kind: 'heartbeat',
    });
    expect(decodeStreamFrame('heartbeat', '')).toEqual({ kind: 'heartbeat' });
  });

  it('event: error 解析 message 与 code', () => {
    expect(
      decodeStreamFrame(
        'error',
        JSON.stringify({ code: 'E_WATCH_FAILED', message: 'upstream watch failed' }),
      ),
    ).toEqual({
      kind: 'error',
      message: 'upstream watch failed',
      code: 'E_WATCH_FAILED',
    });
  });

  it('event: error 只有 code 时用 code 作为展示文案', () => {
    expect(decodeStreamFrame('error', JSON.stringify({ code: 'E_CATCHUP_FAILED' }))).toEqual({
      kind: 'error',
      message: 'E_CATCHUP_FAILED',
      code: 'E_CATCHUP_FAILED',
    });
  });

  it('忽略非法 JSON、缺 event_id 与 seq<=0 的业务帧', () => {
    expect(decodeStreamFrame('harness', 'not-json')).toEqual({ kind: 'ignore' });
    expect(
      decodeStreamFrame('harness', JSON.stringify({ event_type: 'task.started', seq: 1 })),
    ).toEqual({ kind: 'ignore' });
    expect(
      decodeStreamFrame(
        'harness',
        JSON.stringify({ event_id: 'ev-3', event_type: 'task.started', seq: 0 }),
      ),
    ).toEqual({ kind: 'ignore' });
  });

  it('未知 event 名忽略', () => {
    expect(decodeStreamFrame('unknown', JSON.stringify({ event_id: 'ev-4', seq: 1 }))).toEqual({
      kind: 'ignore',
    });
  });
});

describe('mergeLastSeq', () => {
  it('synced 的 last_seq 只推进游标，不回退', () => {
    expect(mergeLastSeq(10, 42)).toBe(42);
    expect(mergeLastSeq(42, 10)).toBe(42);
    expect(mergeLastSeq(42, 0)).toBe(42);
  });

  it('非法 incoming 不改变游标', () => {
    expect(mergeLastSeq(7, Number.NaN)).toBe(7);
    expect(mergeLastSeq(7, undefined as unknown as number)).toBe(7);
  });
});