import type { TaskRuntimeState, TaskStatus } from '../types';
import { statusLabel } from '../types';

const SUMMARY_ITEMS: Array<{ status: TaskStatus; color: string }> = [
  { status: 'running', color: 'var(--hlp-primary, #2bd4a8)' },
  { status: 'waiting_acceptance', color: 'var(--hlp-accent, #4c8dff)' },
  { status: 'retrying', color: 'var(--hlp-warn, #e8b339)' },
  { status: 'failed', color: 'var(--hlp-danger, #e5534b)' },
  { status: 'completed', color: 'var(--hlp-text-secondary, #9aa3ae)' },
];

export interface SummaryBarProps {
  states: TaskRuntimeState[];
}

export default function SummaryBar({ states }: SummaryBarProps) {
  const counts: Partial<Record<TaskStatus, number>> = {};
  for (const state of states) {
    counts[state.status] = (counts[state.status] ?? 0) + 1;
  }

  return (
    <div
      style={{
        display: 'grid',
        gridTemplateColumns: 'repeat(5, minmax(0, 1fr))',
        gap: 10,
      }}
    >
      {SUMMARY_ITEMS.map((item) => (
        <div
          key={item.status}
          style={{
            background: 'var(--hlp-panel, #161a20)',
            border: '1px solid var(--hlp-border, #2a2f38)',
            borderRadius: 'var(--hlp-radius, 10px)',
            padding: '10px 14px',
            boxShadow: 'var(--hlp-shadow, 0 1px 2px rgba(0, 0, 0, 0.35))',
          }}
        >
          <div
            style={{
              display: 'flex',
              alignItems: 'center',
              gap: 8,
              color: 'var(--hlp-text-secondary, #9aa3ae)',
              fontSize: 12,
            }}
          >
            <span
              aria-hidden="true"
              style={{
                width: 6,
                height: 6,
                borderRadius: '50%',
                background: item.color,
              }}
            />
            {statusLabel(item.status)}
          </div>
          <div
            style={{
              marginTop: 6,
              fontSize: 24,
              fontWeight: 600,
              lineHeight: 1.1,
              color: 'var(--hlp-text, #e6e9ef)',
              fontVariantNumeric: 'tabular-nums',
            }}
          >
            {counts[item.status] ?? 0}
          </div>
        </div>
      ))}
    </div>
  );
}