import type { DagProgress } from '../subtask';
import { SUBTASK_STATUS_LABELS } from '../subtask';

const SEGMENTS: Array<{ status: keyof DagProgress['counts']; color: string }> = [
  { status: 'accepted', color: 'var(--hlp-primary, #2bd4a8)' },
  { status: 'running', color: 'var(--hlp-accent, #4c8dff)' },
  { status: 'failed', color: 'var(--hlp-danger, #e5534b)' },
  { status: 'cancelled', color: 'var(--hlp-muted, #6b7280)' },
  { status: 'queued', color: 'var(--hlp-border, #2a2f38)' },
];

export interface DagProgressBarProps {
  progress: DagProgress;
}

export default function DagProgressBar({ progress }: DagProgressBarProps) {
  const total = Math.max(1, progress.total);

  return (
    <div>
      <div
        style={{
          display: 'flex',
          alignItems: 'baseline',
          justifyContent: 'space-between',
          marginBottom: 8,
          fontSize: 12,
          color: 'var(--hlp-text-secondary, #9aa3ae)',
        }}
      >
        <span>
          已验收 <span style={{ color: 'var(--hlp-text, #e6e9ef)' }}>{progress.counts.accepted}</span>
          {' / '}
          {progress.total} 个节点 · {progress.acceptedPercent}%
        </span>
        <span>
          失败 {progress.counts.failed} · 运行 {progress.counts.running}
        </span>
      </div>

      <div
        style={{
          display: 'flex',
          width: '100%',
          height: 10,
          overflow: 'hidden',
          borderRadius: 5,
          background: 'var(--hlp-panel-strong, #1c2129)',
          border: '1px solid var(--hlp-border, #2a2f38)',
          boxSizing: 'border-box',
        }}
        role="img"
        aria-label={`DAG 进度：已验收 ${progress.counts.accepted}，运行 ${progress.counts.running}，失败 ${progress.counts.failed}，取消 ${progress.counts.cancelled}，排队 ${progress.counts.queued}`}
      >
        {SEGMENTS.map((segment) => {
          const count = progress.counts[segment.status];
          if (count === 0) {
            return null;
          }
          return (
            <div
              key={segment.status}
              title={`${SUBTASK_STATUS_LABELS[segment.status]} ${count}`}
              style={{
                width: `${(count / total) * 100}%`,
                height: '100%',
                background: segment.color,
                opacity: 0.92,
              }}
            />
          );
        })}
      </div>

      <div
        style={{
          display: 'flex',
          flexWrap: 'wrap',
          gap: 12,
          marginTop: 8,
          fontSize: 11,
          color: 'var(--hlp-text-secondary, #9aa3ae)',
        }}
      >
        {SEGMENTS.map((segment) => (
          <span key={segment.status} style={{ display: 'inline-flex', alignItems: 'center', gap: 5 }}>
            <span
              aria-hidden="true"
              style={{
                width: 8,
                height: 8,
                borderRadius: 2,
                background: segment.color,
                display: 'inline-block',
              }}
            />
            {SUBTASK_STATUS_LABELS[segment.status]} {progress.counts[segment.status]}
          </span>
        ))}
      </div>

      {progress.failedNodes.length > 0 && (
        <div style={{ marginTop: 8, fontSize: 11, color: 'var(--hlp-danger, #e5534b)' }}>
          失败节点：{progress.failedNodes.join('、')}
        </div>
      )}
    </div>
  );
}