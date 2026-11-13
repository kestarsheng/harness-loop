import { useState } from 'react';
import { Descriptions, Empty, Tag } from 'antd';
import type { HandoffContractSummary } from '../subtask';

export interface HandoffGraphProps {
  handoffs: HandoffContractSummary[];
}

function formatTime(value: number): string {
  if (!Number.isFinite(value) || value <= 0) {
    return '—';
  }
  return new Date(value).toLocaleTimeString('zh-CN', { hour12: false });
}

export default function HandoffGraph({ handoffs }: HandoffGraphProps) {
  const [selectedId, setSelectedId] = useState<string | null>(
    handoffs.length > 0 ? handoffs[0].handoffId : null,
  );
  const selected = handoffs.find((entry) => entry.handoffId === selectedId) ?? null;

  if (handoffs.length === 0) {
    return <Empty description="暂无 handoff 契约" image={Empty.PRESENTED_IMAGE_SIMPLE} />;
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
                style={{
                  marginInlineEnd: 0,
                  fontSize: 10,
                  lineHeight: '16px',
                  background: 'transparent',
                  borderColor: 'var(--hlp-border, #2a2f38)',
                  color: 'var(--hlp-text-secondary, #9aa3ae)',
                }}
              >
                {handoff.fromRole}
              </Tag>
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
              <Tag
                style={{
                  marginInlineEnd: 0,
                  fontSize: 10,
                  lineHeight: '16px',
                  background: 'transparent',
                  borderColor: 'var(--hlp-border, #2a2f38)',
                  color: 'var(--hlp-text-secondary, #9aa3ae)',
                }}
              >
                {handoff.toRole}
              </Tag>
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
                {handoff.scope}
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
          <Descriptions.Item label="方向">
            {selected.fromRole} → {selected.toRole}
          </Descriptions.Item>
          <Descriptions.Item label="范围">{selected.scope}</Descriptions.Item>
          <Descriptions.Item label="产物">
            {selected.artifactCount} 项 · 建立于 {formatTime(selected.createdAtMs)}
          </Descriptions.Item>
          <Descriptions.Item label="验收条款">
            <div style={{ display: 'flex', flexWrap: 'wrap', gap: 4 }}>
              {selected.acceptedCriteria.map((criterion) => (
                <Tag key={criterion} style={{ marginInlineEnd: 0, fontSize: 10 }}>
                  {criterion}
                </Tag>
              ))}
            </div>
          </Descriptions.Item>
        </Descriptions>
      )}

      <div style={{ fontSize: 11, color: 'var(--hlp-muted, #6b7280)', lineHeight: 1.6 }}>
        只展示契约摘要（角色、范围、产物数、验收条款），不展示原始对话与完整产物内容。
      </div>
    </div>
  );
}