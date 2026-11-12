import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import AgentCard from './AgentCard';
import type { TaskRuntimeState } from '../types';

const CARD_HEIGHT = 148;
const CARD_MIN_WIDTH = 300;
const GRID_GAP = 12;
const ROW_HEIGHT = CARD_HEIGHT + GRID_GAP;
const OVERSCAN_ROWS = 2;

export interface AgentCardGridProps {
  states: TaskRuntimeState[];
  now: number;
  selectedTaskId: string | null;
  onSelect: (taskId: string) => void;
  onOpenAnomaly: (taskId: string, traceId?: string) => void;
  emptyPlaceholder?: ReactNode;
}

export default function AgentCardGrid({
  states,
  now,
  selectedTaskId,
  onSelect,
  onOpenAnomaly,
  emptyPlaceholder,
}: AgentCardGridProps) {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const [scrollTop, setScrollTop] = useState(0);
  const [viewport, setViewport] = useState({ width: 0, height: 0 });

  useEffect(() => {
    const node = containerRef.current;
    if (!node) {
      return undefined;
    }
    const measure = () => {
      setViewport({ width: node.clientWidth, height: node.clientHeight });
    };
    measure();
    if (typeof ResizeObserver === 'undefined') {
      window.addEventListener('resize', measure);
      return () => window.removeEventListener('resize', measure);
    }
    const observer = new ResizeObserver(measure);
    observer.observe(node);
    return () => observer.disconnect();
  }, []);

  const handleScroll = useCallback((event: React.UIEvent<HTMLDivElement>) => {
    setScrollTop(event.currentTarget.scrollTop);
  }, []);

  const columns = useMemo(() => {
    const width = viewport.width || 900;
    return Math.max(1, Math.floor((width + GRID_GAP) / (CARD_MIN_WIDTH + GRID_GAP)));
  }, [viewport.width]);

  const totalRows = Math.ceil(states.length / columns);
  const visibleHeight = viewport.height || 600;
  const startRow = Math.max(0, Math.floor(scrollTop / ROW_HEIGHT) - OVERSCAN_ROWS);
  const endRow = Math.min(
    totalRows,
    Math.ceil((scrollTop + visibleHeight) / ROW_HEIGHT) + OVERSCAN_ROWS,
  );

  const visibleStates = useMemo(() => {
    const startIndex = startRow * columns;
    const endIndex = Math.min(states.length, endRow * columns);
    return states.slice(startIndex, endIndex);
  }, [states, columns, startRow, endRow]);

  if (states.length === 0) {
    return (
      <div
        style={{
          height: '100%',
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          color: 'var(--hlp-text-secondary, #9aa3ae)',
          fontSize: 13,
        }}
      >
        {emptyPlaceholder ?? '暂无运行中的任务'}
      </div>
    );
  }

  return (
    <div
      ref={containerRef}
      onScroll={handleScroll}
      style={{
        height: '100%',
        overflowY: 'auto',
        overflowX: 'hidden',
        paddingRight: 4,
      }}
    >
      <div
        style={{
          position: 'relative',
          height: totalRows * ROW_HEIGHT,
        }}
      >
        <div
          style={{
            position: 'absolute',
            top: startRow * ROW_HEIGHT,
            left: 0,
            right: 0,
            display: 'grid',
            gridTemplateColumns: `repeat(${columns}, minmax(0, 1fr))`,
            gap: GRID_GAP,
          }}
        >
          {visibleStates.map((state) => (
            <AgentCard
              key={state.taskId}
              state={state}
              now={now}
              selected={selectedTaskId === state.taskId}
              onSelect={onSelect}
              onOpenAnomaly={onOpenAnomaly}
            />
          ))}
        </div>
      </div>
    </div>
  );
}