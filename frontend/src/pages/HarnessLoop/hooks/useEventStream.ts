import { useEffect, useRef } from 'react';
import harnessLoopApi from '../api/harnessLoop';
import type { HarnessEvent } from '../types';
import { toNumber } from '../types';

export type StreamStatus = 'connecting' | 'open' | 'reconnecting' | 'degraded' | 'closed';

export interface UseEventStreamOptions {
  tenantId?: string;
  enabled?: boolean;
  getLastSeq: () => number;
  onEvents: (events: HarnessEvent[]) => void;
  onStatus?: (status: StreamStatus) => void;
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

    const handleEventPayload = (raw: string) => {
      try {
        const parsed = JSON.parse(raw) as HarnessEvent;
        const seq = toNumber(parsed.seq);
        if (!parsed.event_id || seq <= 0) {
          return;
        }
        if (lastSeqRef.current > 0 && seq > lastSeqRef.current + 1) {
          void pullGap(lastSeqRef.current);
        }
        bufferRef.current.push(parsed);
        scheduleFlush();
      } catch {
        // 忽略无法解析的帧，等待下一次心跳
      }
    };

    const connect = () => {
      if (disposedRef.current) {
        return;
      }
      sourceRef.current?.close();
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

        source.onmessage = (message: MessageEvent<string>) => {
          handleEventPayload(message.data);
        };

        source.addEventListener('harness', (message) => {
          handleEventPayload((message as MessageEvent<string>).data);
        });

        source.addEventListener('heartbeat', () => {
          attemptRef.current = 0;
        });

        source.addEventListener('synced', () => {
          attemptRef.current = 0;
          void pullGap(lastSeqRef.current);
        });

        source.onerror = () => {
          if (disposedRef.current) {
            return;
          }
          source.close();
          sourceRef.current = null;
          scheduleReconnect();
        };
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
      sourceRef.current?.close();
      sourceRef.current = null;
      bufferRef.current = [];
      attemptRef.current = 0;
      degradedRef.current = false;
      emitStatus('closed');
    };
  }, [tenantId, enabled]);

}

export default useEventStream;