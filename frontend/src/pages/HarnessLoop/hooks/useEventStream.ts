import { useEffect, useRef } from 'react';
import harnessLoopApi from '../api/harnessLoop';
import type { HarnessEvent } from '../types';
import { toNumber } from '../types';
import { decodeStreamFrame, mergeLastSeq, STREAM_EVENT_NAME } from '../streamProtocol';

export type StreamStatus = 'connecting' | 'open' | 'reconnecting' | 'degraded' | 'closed';

export interface UseEventStreamOptions {
  tenantId?: string;
  enabled?: boolean;
  getLastSeq: () => number;
  onEvents: (events: HarnessEvent[]) => void;
  onStatus?: (status: StreamStatus) => void;
  onError?: (message: string) => void;
}

const FLUSH_INTERVAL_MS = 200;
const FLUSH_INTERVAL_HIDDEN_MS = 2000;
const RECONNECT_BASE_MS = 1000;
const RECONNECT_MAX_MS = 30000;
const MAX_RECONNECT_ATTEMPTS = 6;
const FALLBACK_POLL_MS = 5000;
const GAP_FETCH_LIMIT = 500;

function sortBySeq(events: HarnessEvent[]): HarnessEvent[] {
  return [...events].sort((left, right) => toNumber(left.seq) - toNumber(right.seq));
}

export function useEventStream(options: UseEventStreamOptions) {
  const { tenantId = 'default', enabled = true } = options;

  const optionsRef = useRef(options);

  useEffect(() => {
    optionsRef.current = options;
  });

  const sourceRef = useRef<EventSource | null>(null);
  const bufferRef = useRef<HarnessEvent[]>([]);
  const flushTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const reconnectTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const pollTimerRef = useRef<ReturnType<typeof setInterval> | null>(null);
  const attemptRef = useRef(0);
  const lastSeqRef = useRef(0);
  const statusRef = useRef<StreamStatus>('closed');
  const disposedRef = useRef(false);
  const degradedRef = useRef(false);
  const pullingRef = useRef(false);
  const flushDelayRef = useRef(FLUSH_INTERVAL_MS);

  useEffect(() => {
    disposedRef.current = false;
    lastSeqRef.current = optionsRef.current.getLastSeq?.() ?? 0;

    const emitStatus = (status: StreamStatus) => {
      if (statusRef.current === status) {
        return;
      }
      statusRef.current = status;
      optionsRef.current.onStatus?.(status);
    };

    const pushEvents = (events: HarnessEvent[]) => {
      if (events.length === 0) {
        return;
      }
      optionsRef.current.onEvents(events);
      const maxSeq = events.reduce((max, event) => Math.max(max, toNumber(event.seq)), 0);
      lastSeqRef.current = Math.max(lastSeqRef.current, maxSeq);
    };

    const flush = () => {
      flushTimerRef.current = null;
      if (disposedRef.current || bufferRef.current.length === 0) {
        return;
      }
      const deduped = new Map<string, HarnessEvent>();
      for (const event of bufferRef.current) {
        deduped.set(event.event_id, event);
      }
      bufferRef.current = [];
      pushEvents(sortBySeq([...deduped.values()]));
    };

    const scheduleFlush = () => {
      if (flushTimerRef.current) {
        return;
      }
      // 保持 200ms 批量渲染：一次 flush 只触发一次 React 状态更新。
      flushTimerRef.current = setTimeout(flush, flushDelayRef.current);
    };

    const pullGap = async (fromSeq: number) => {
      if (pullingRef.current || disposedRef.current) {
        return;
      }
      pullingRef.current = true;
      try {
        const response = await harnessLoopApi.listEvents({
          since_seq: fromSeq,
          limit: GAP_FETCH_LIMIT,
        });
        const events = (response?.events ?? []) as HarnessEvent[];
        if (events.length > 0 && !disposedRef.current) {
          pushEvents(sortBySeq(events));
        }
      } catch {
        // 补拉失败交由下一次重连或轮询兜底
      } finally {
        pullingRef.current = false;
      }
    };

    const startFallbackPolling = () => {
      if (pollTimerRef.current || disposedRef.current) {
        return;
      }
      degradedRef.current = true;
      emitStatus('degraded');
      pollTimerRef.current = setInterval(() => {
        void pullGap(lastSeqRef.current);
      }, FALLBACK_POLL_MS);
    };

    const stopFallbackPolling = () => {
      if (pollTimerRef.current) {
        clearInterval(pollTimerRef.current);
        pollTimerRef.current = null;
      }
    };

    const closeSource = () => {
      if (sourceRef.current) {
        sourceRef.current.close();
        sourceRef.current = null;
      }
    };

    const scheduleReconnect = () => {
      if (disposedRef.current) {
        return;
      }
      attemptRef.current += 1;
      if (attemptRef.current >= MAX_RECONNECT_ATTEMPTS) {
        startFallbackPolling();
        return;
      }
      emitStatus('reconnecting');
      const delay = Math.min(
        RECONNECT_BASE_MS * 2 ** (attemptRef.current - 1),
        RECONNECT_MAX_MS,
      );
      const jitter = Math.floor(Math.random() * 250);
      reconnectTimerRef.current = setTimeout(() => {
        reconnectTimerRef.current = null;
        connect();
      }, delay + jitter);
    };

    const handleFrame = (eventName: string, raw: string) => {
      const frame = decodeStreamFrame(eventName, raw);
      switch (frame.kind) {
        case 'event': {
          const parsed = frame.event;
          const seq = toNumber(parsed.seq);
          if (lastSeqRef.current > 0 && seq > lastSeqRef.current + 1) {
            void pullGap(lastSeqRef.current);
          }
          bufferRef.current.push(parsed);
          scheduleFlush();
          break;
        }
        case 'synced':
          // Gateway 已完成 last_seq 之后的补拉，前端不再重复补拉。
          // 只推进游标：补拉期间已达的更高 seq 不能被覆盖回退。
          attemptRef.current = 0;
          lastSeqRef.current = mergeLastSeq(lastSeqRef.current, frame.lastSeq);
          stopFallbackPolling();
          degradedRef.current = false;
          emitStatus('open');
          break;
        case 'heartbeat':
          // 心跳不产生业务事件，只证明连接仍然有效。
          attemptRef.current = 0;
          if (statusRef.current !== 'open') {
            stopFallbackPolling();
            degradedRef.current = false;
            emitStatus('open');
          }
          break;
        case 'error':
          // Gateway 在补拉失败或上游 WatchEvents 失败时发送 event: error。
          // 展示错误并主动重连；重连会携带当前 last_seq，避免重复计数。
          optionsRef.current.onError?.(frame.message);
          closeSource();
          scheduleReconnect();
          break;
        case 'ignore':
        default:
          break;
      }
    };

    const connect = () => {
      if (disposedRef.current) {
        return;
      }
      closeSource();
      emitStatus(attemptRef.current === 0 ? 'connecting' : 'reconnecting');
      try {
        const source = new EventSource(
          harnessLoopApi.streamUrl(tenantId, lastSeqRef.current),
        );
        sourceRef.current = source;

        source.onopen = () => {
          if (disposedRef.current) {
            return;
          }
          attemptRef.current = 0;
          stopFallbackPolling();
          degradedRef.current = false;
          emitStatus('open');
        };

        source.addEventListener(STREAM_EVENT_NAME, (message) => {
          handleFrame(STREAM_EVENT_NAME, (message as MessageEvent<string>).data);
        });

        // 兼容没有 event 名的旧服务端帧。
        source.onmessage = (message: MessageEvent<string>) => {
          handleFrame('', message.data);
        };

        source.addEventListener('heartbeat', (message) => {
          handleFrame('heartbeat', (message as MessageEvent<string>).data);
        });

        source.addEventListener('synced', (message) => {
          handleFrame('synced', (message as MessageEvent<string>).data);
        });

        source.addEventListener('error', (event) => {
          if (disposedRef.current) {
            return;
          }
          const data = (event as MessageEvent<string>).data;
          if (typeof data === 'string' && data !== '') {
            // 服务端 event: error 帧，展示并重连。
            handleFrame('error', data);
            return;
          }
          // 传输层错误：EventSource 已断开，按指数退避重连。
          closeSource();
          scheduleReconnect();
        });
      } catch {
        scheduleReconnect();
      }
    };

    const handleVisibilityChange = () => {
      if (document.hidden) {
        flushDelayRef.current = FLUSH_INTERVAL_HIDDEN_MS;
      } else {
        flushDelayRef.current = FLUSH_INTERVAL_MS;
        if (bufferRef.current.length > 0) {
          scheduleFlush();
        }
        if (degradedRef.current) {
          void pullGap(lastSeqRef.current);
        }
      }
    };

    if (enabled) {
      if (typeof EventSource === 'undefined') {
        startFallbackPolling();
      } else {
        connect();
      }
      document.addEventListener('visibilitychange', handleVisibilityChange);
    }

    return () => {
      disposedRef.current = true;
      document.removeEventListener('visibilitychange', handleVisibilityChange);
      if (flushTimerRef.current) {
        clearTimeout(flushTimerRef.current);
        flushTimerRef.current = null;
      }
      if (reconnectTimerRef.current) {
        clearTimeout(reconnectTimerRef.current);
        reconnectTimerRef.current = null;
      }
      stopFallbackPolling();
      closeSource();
      bufferRef.current = [];
      attemptRef.current = 0;
      degradedRef.current = false;
      emitStatus('closed');
    };
  }, [tenantId, enabled]);

}

export default useEventStream;