import type { HarnessEvent } from './types';
import { toNumber } from './types';

/**
 * Gateway SSE 帧：
 * - event: harness  业务事件（id = event_id，data 为事件 JSON）
 * - event: synced   last_seq 之前的补拉已完成
 * - event: heartbeat 仅维持连接，不携带业务数据
 * - event: error    补拉或上游 WatchEvents 失败，客户端应展示并可重连
 * 未写 event 名的帧按业务事件处理，兼容旧实现。
 */
export const STREAM_EVENT_NAME = 'harness';

export type StreamFrame =
  | { kind: 'event'; event: HarnessEvent }
  | { kind: 'synced'; lastSeq: number }
  | { kind: 'heartbeat' }
  | { kind: 'error'; message: string; code: string }
  | { kind: 'ignore' };

function parseJson(raw: string): Record<string, unknown> {
  if (typeof raw !== 'string' || raw.trim() === '') {
    return {};
  }
  try {
    const parsed = JSON.parse(raw);
    return parsed && typeof parsed === 'object' ? (parsed as Record<string, unknown>) : {};
  } catch {
    return {};
  }
}

export function decodeStreamFrame(eventName: string, raw: string): StreamFrame {
  const name = (eventName || '').trim();
  const data = parseJson(raw);

  if (name === 'heartbeat') {
    return { kind: 'heartbeat' };
  }
  if (name === 'synced') {
    return { kind: 'synced', lastSeq: toNumber(data.last_seq) };
  }
  if (name === 'error') {
    const message =
      (typeof data.message === 'string' && data.message.trim()) ||
      (typeof data.code === 'string' && data.code.trim()) ||
      '实时通道错误';
    const code = typeof data.code === 'string' ? data.code : '';
    return { kind: 'error', message, code };
  }
  if (name !== '' && name !== STREAM_EVENT_NAME) {
    return { kind: 'ignore' };
  }

  const event = data as unknown as HarnessEvent;
  if (!event || typeof event.event_id !== 'string' || event.event_id === '') {
    return { kind: 'ignore' };
  }
  if (toNumber(event.seq) <= 0) {
    return { kind: 'ignore' };
  }
  return { kind: 'event', event };
}

/**
 * synced 帧的 last_seq 只能推进游标，不能回退。
 * 补拉期间业务事件可能已经把游标推到更后面。
 */
export function mergeLastSeq(current: number, incoming: number): number {
  return Math.max(current, toNumber(incoming));
}