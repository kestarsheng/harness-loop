import { memo } from 'react';
import { Tag, Tooltip } from 'antd';
import { WarningOutlined } from '@ant-design/icons';
import type { StageCode, TaskRuntimeState } from '../types';
import {
  formatDuration,
  healthLevel,
  isTerminalStatus,
  STAGES,
  statusLabel,
} from '../types';

const STAGE_ORDER: Record<StageCode, number> = { a: 0, b: 1, c: 2, d: 3, e: 4 };

const STATUS_COLORS: Record<string, string> = {
  pending: 'default',
  running: 'processing',
  waiting_acceptance: 'gold',
  retrying: 'orange',
  paused: 'default',
  completed: 'success',
  failed: 'error',
  aborted: 'default',
};

const HEALTH_COLORS = {
  ok: 'var(--hlp-primary, #2bd4a8)',
  warn: 'var(--hlp-warn, #e8b339)',
  danger: 'var(--hlp-danger, #e5534b)',
};

const VERDICT_LABELS: Record<string, string> = {
  passed: '验收通过',
  failed: '验收未通过',
  invalid: '验收记录非法',
};

export interface AgentCardProps {
  state: TaskRuntimeState;
  now: number;
  selected: boolean;
  onSelect: (taskId: string) => void;
  onOpenAnomaly: (taskId: string, traceId?: string) => void;
}

function rollbackCount(state: TaskRuntimeState): number {
  let count = 0;
  for (const transition of state.transitions) {
    const from = STAGE_ORDER[transition.from as StageCode];
    const to = STAGE_ORDER[transition.to as StageCode];
    if (from !== undefined && to !== undefined && to < from) {
      count += 1;
    }
  }
  return count;
}

function AgentCardComponent({
  state,
  now,
  selected,
  onSelect,
  onOpenAnomaly,
}: AgentCardProps) {
  const level = healthLevel(state.healthScore);
  const healthColor = HEALTH_COLORS[level];
  const currentStage = state.stage ?? null;
  const attemptNo = currentStage ? state.attemptNoByStage[currentStage] : 0;
  const rollbacks = rollbackCount(state);
  const endedAt = isTerminalStatus(state.status) ? state.updatedAtMs : now;
  const elapsed = state.startedAtMs ? endedAt - state.startedAtMs : 0;
  const anomalyCount = state.anomalies.length;
  const latestAnomaly = state.anomalies[0];

  return (
    <div
      role="button"
      tabIndex={0}
      aria-label={`任务 ${state.taskId}，${statusLabel(state.status)}`}
      onClick={() => onSelect(state.taskId)}
      onKeyDown={(event) => {
        if (event.key === 'Enter' || event.key === ' ') {
          event.preventDefault();
          onSelect(state.taskId);
        }
      }}
      style={{
        position: 'relative',
        height: 148,
        boxSizing: 'border-box',
        padding: '12px 14px',
        background: selected
          ? 'var(--hlp-panel-strong, #1c2129)'
          : 'var(--hlp-panel, #161a20)',
        border: selected
          ? '1px solid var(--hlp-primary, #2bd4a8)'
          : '1px solid var(--hlp-border, #2a2f38)',
        borderRadius: 'var(--hlp-radius, 10px)',
        boxShadow: 'var(--hlp-shadow, 0 1px 2px rgba(0, 0, 0, 0.35))',
        cursor: 'pointer',
        outline: 'none',
        transition: 'border-color 120ms ease, background 120ms ease',
      }}
    >
      <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
        <span
          style={{
            fontFamily:
              'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
            fontSize: 12,
            color: 'var(--hlp-text, #e6e9ef)',
            overflow: 'hidden',
            textOverflow: 'ellipsis',
            whiteSpace: 'nowrap',
            flex: 1,
          }}
          title={state.taskId}
        >
          {state.taskId.slice(0, 12)}
        </span>
        {anomalyCount > 0 && (
          <Tooltip title={`${anomalyCount} 条异常，点击定位`}>
            <button
              type="button"
              aria-label="查看异常"
              onClick={(event) => {
                event.stopPropagation();
                onOpenAnomaly(state.taskId, latestAnomaly?.traceId);
              }}
              style={{
                display: 'inline-flex',
                alignItems: 'center',
                gap: 4,
                border: 'none',
                background: 'transparent',
                color: 'var(--hlp-danger, #e5534b)',
                fontSize: 11,
                cursor: 'pointer',
                padding: 0,
              }}
            >
              <WarningOutlined />
              {anomalyCount}
            </button>
          </Tooltip>
        )}
        <Tag
          color={STATUS_COLORS[state.status] ?? 'default'}
          style={{ marginInlineEnd: 0, fontSize: 11, lineHeight: '18px' }}
        >
          {statusLabel(state.status)}
        </Tag>
      </div>

      <div style={{ display: 'flex', alignItems: 'baseline', gap: 6, marginTop: 10 }}>
        <span style={{ fontSize: 18, fontWeight: 600, color: 'var(--hlp-text, #e6e9ef)' }}>
          {currentStage ? currentStage.toUpperCase() : '—'}
        </span>
        <span style={{ fontSize: 11, color: 'var(--hlp-text-secondary, #9aa3ae)' }}>
          阶段
        </span>
        <span style={{ fontSize: 12, color: 'var(--hlp-text-secondary, #9aa3ae)' }}>
          attempt {attemptNo || 0}
        </span>
        {rollbacks > 0 && (
          <Tooltip title={`回退 ${rollbacks} 次`}>
            <Tag color="warning" style={{ marginInlineStart: 4, fontSize: 10, lineHeight: '16px' }}>
              回退 {rollbacks}
            </Tag>
          </Tooltip>
        )}
      </div>

      <div style={{ display: 'flex', gap: 2, marginTop: 8 }} aria-hidden="true">
        {STAGES.map((stage) => {
          const stageState = state.stages[stage];
          const active = currentStage === stage;
          return (
            <div
              key={stage}
              title={`${stage.toUpperCase()} 阶段，尝试 ${stageState.lastAttempt} 次`}
              style={{
                flex: 1,
                height: 4,
                borderRadius: 2,
                background:
                  stageState.attempts > 0
                    ? `var(--hlp-stage-${stage}, #6b7280)`
                    : 'var(--hlp-border, #2a2f38)',
                opacity: active ? 1 : stageState.attempts > 0 ? 0.55 : 1,
              }}
            />
          );
        })}
      </div>

      <div
        style={{
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          marginTop: 10,
          gap: 8,
        }}
      >
        <div style={{ display: 'flex', alignItems: 'baseline', gap: 6 }}>
          <span
            style={{
              fontSize: 16,
              fontWeight: 600,
              color: healthColor,
              fontVariantNumeric: 'tabular-nums',
            }}
          >
            {state.healthScore}
          </span>
          <span style={{ fontSize: 11, color: 'var(--hlp-text-secondary, #9aa3ae)' }}>
            健康度
          </span>
        </div>
        <div style={{ fontSize: 11, color: 'var(--hlp-text-secondary, #9aa3ae)' }}>
          {state.recentVerdict
            ? VERDICT_LABELS[state.recentVerdict]
            : '验收样本不足'}
        </div>
        <div
          style={{
            fontSize: 11,
            color: 'var(--hlp-text-secondary, #9aa3ae)',
            fontVariantNumeric: 'tabular-nums',
          }}
        >
          {formatDuration(elapsed)}
        </div>
      </div>
    </div>
  );
}

const AgentCard = memo(AgentCardComponent);

export default AgentCard;