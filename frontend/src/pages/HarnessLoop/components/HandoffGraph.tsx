import { useState } from 'react';
import { Descriptions, Empty, Tag } from 'antd';
import type { HandoffRowView } from '../adapters';

export interface HandoffGraphProps {
  handoffs: HandoffRowView[];
}

const STATUS_COLORS: Record<string, string> = {
  created: 'default',
  accepted: 'success',
  rejected: 'error',
};

function agentLabel(value: string): string {
  return value || '未提供';
}

function formatTime(value: number): string {
  if (!Number.isFinite(value) || value <= 0) {
    return '—';
  }
  return new Date(value).toLocaleString('zh-CN', { hour12: false });
}

export default function HandoffGraph({ handoffs }: HandoffGraphProps) {
  const [selectedId, setSelectedId] = useState<string | null>(
    handoffs.length > 0 ? handoffs[0].handoffId : null,
  );
  const selected = handoffs.find((entry) => entry.handoffId === selectedId) ?? null;

  if (handoffs.length === 0) {
    return (
      <Empty
        description="暂无 handoff 记录（该任务的事件流中没有 handoff.* 事件）"
        image={Empty.PRESENTED_IMAGE_SIMPLE}
      />
    );
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
        {handoffs.map((handoff) => {
          const active = handoff.handoffId === selectedId;
          return (
            <button
              key={handoff.handoffId}
              type="button"
              onClick={() => setSelectedId(handoff.handoffId)}
              aria-pressed={active}
              style={{
                display: 'flex',
                alignItems: 'center',
                gap: 8,
                width: '100%',
                padding: '6px 8px',
                background: active
                  ? 'var(--hlp-panel-strong, #1c2129)'
                  : 'transparent',
                border: active
                  ? '1px solid var(--hlp-primary, #2bd4a8)'
                  : '1px solid var(--hlp-border, #2a2f38)',
                borderRadius: 6,
                cursor: 'pointer',
                textAlign: 'left',
                color: 'var(--hlp-text, #e6e9ef)',
              }}
            >
              <Tag
                color={STATUS_COLORS[handoff.status] ?? 'default'}
                style={{ marginInlineEnd: 0, fontSize: 10, lineHeight: '16px' }}
              >
                {handoff.status}
              </Tag>
              <span
                style={{
                  fontSize: 11,
                  color: 'var(--hlp-text-secondary, #9aa3ae)',
                  maxWidth: 120,
                  overflow: 'hidden',
                  textOverflow: 'ellipsis',
                  whiteSpace: 'nowrap',
                }}
                title={handoff.fromAgentId || undefined}
              >
                {agentLabel(handoff.fromAgentId)}
              </span>
              <span
                aria-hidden="true"
                style={{
                  flex: 1,
                  position: 'relative',
                  height: 1,
                  background: 'var(--hlp-border, #2a2f38)',
                }}
              >
                <span
                  style={{
                    position: 'absolute',
                    right: -2,
                    top: -6,
                    fontSize: 9,
                    color: 'var(--hlp-text-secondary, #9aa3ae)',
                  }}
                >
                  ▶
                </span>
              </span>
              <span
                style={{
                  fontSize: 11,
                  color: 'var(--hlp-text-secondary, #9aa3ae)',
                  maxWidth: 120,
                  overflow: 'hidden',
                  textOverflow: 'ellipsis',
                  whiteSpace: 'nowrap',
                }}
                title={handoff.toAgentId || undefined}
              >
                {agentLabel(handoff.toAgentId)}
              </span>
              <span
                style={{
                  fontSize: 10,
                  color: 'var(--hlp-muted, #6b7280)',
                  whiteSpace: 'nowrap',
                  overflow: 'hidden',
                  textOverflow: 'ellipsis',
                  maxWidth: 160,
                }}
              >
                {handoff.reason || handoff.inputDigest || handoff.handoffId}
              </span>
            </button>
          );
        })}
      </div>

      {selected && (
        <Descriptions size="small" column={1} bordered style={{ fontSize: 12 }}>
          <Descriptions.Item label="契约">
            <span style={{ fontFamily: 'ui-monospace, monospace', fontSize: 11 }}>
              {selected.handoffId}
            </span>
          </Descriptions.Item>
          <Descriptions.Item label="发起 Agent">
            <span style={{ fontFamily: 'ui-monospace, monospace', fontSize: 11 }}>
              {agentLabel(selected.fromAgentId)}
            </span>
          </Descriptions.Item>
          <Descriptions.Item label="接收 Agent">
            <span style={{ fontFamily: 'ui-monospace, monospace', fontSize: 11 }}>
              {agentLabel(selected.toAgentId)}
            </span>
          </Descriptions.Item>
          <Descriptions.Item label="状态">
            <Tag color={STATUS_COLORS[selected.status] ?? 'default'}>
              {selected.status}
            </Tag>
          </Descriptions.Item>
          <Descriptions.Item label="原因">
            {selected.reason || '—'}
          </Descriptions.Item>
          <Descriptions.Item label="输入摘要">
            <span style={{ fontFamily: 'ui-monospace, monospace', fontSize: 11 }}>
              {selected.inputDigest || '—'}
            </span>
          </Descriptions.Item>
          <Descriptions.Item label="时间">
            {formatTime(selected.createdAtMs)}
            {selected.updatedAtMs > selected.createdAtMs
              ? ` · 更新于 ${formatTime(selected.updatedAtMs)}`
              : ''}
          </Descriptions.Item>
        </Descriptions>
      )}

      <div style={{ fontSize: 11, color: 'var(--hlp-muted, #6b7280)', lineHeight: 1.6 }}>
        数据由后端把 handoff.* 事件按 handoff_id 折叠得到；后端无角色字段，因此只展示 agent id、状态与摘要。
      </div>
    </div>
  );
}