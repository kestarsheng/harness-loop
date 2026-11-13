import { useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { Empty, Tag } from 'antd';
import type { SubtaskSnapshot, SubtaskStatus, SubtaskTreeNode } from '../subtask';
import {
  SUBTASK_STATUS_LABELS,
  buildSubtaskTree,
} from '../subtask';
import { formatDuration } from '../types';

const STATUS_COLORS: Record<SubtaskStatus, string> = {
  queued: 'default',
  running: 'processing',
  accepted: 'success',
  failed: 'error',
  cancelled: 'default',
};

const STATUS_DOTS: Record<SubtaskStatus, string> = {
  queued: 'var(--hlp-muted, #6b7280)',
  running: 'var(--hlp-accent, #4c8dff)',
  accepted: 'var(--hlp-primary, #2bd4a8)',
  failed: 'var(--hlp-danger, #e5534b)',
  cancelled: 'var(--hlp-text-secondary, #9aa3ae)',
};

export interface SubagentTreeProps {
  snapshot: SubtaskSnapshot;
  nowMs: number;
}

export default function SubagentTree({ snapshot, nowMs }: SubagentTreeProps) {
  const tree = useMemo(() => buildSubtaskTree(snapshot, 2), [snapshot]);
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({});

  if (tree.length === 0) {
    return <Empty description="该任务暂无子任务" image={Empty.PRESENTED_IMAGE_SIMPLE} />;
  }

  const toggle = (subtaskId: string) => {
    setCollapsed((previous) => ({ ...previous, [subtaskId]: !previous[subtaskId] }));
  };

  const renderNode = (item: SubtaskTreeNode): ReactNode => {
    const { node, depth, children } = item;
    const isCollapsed = Boolean(collapsed[node.subtaskId]);
    const endedAt = node.endedAtMs ?? (node.status === 'running' ? nowMs : null);
    const elapsed =
      node.startedAtMs !== null && endedAt !== null ? endedAt - node.startedAtMs : 0;

    return (
      <div key={node.subtaskId} style={{ marginTop: depth === 0 ? 6 : 4 }}>
        <div
          style={{
            display: 'flex',
            alignItems: 'center',
            gap: 8,
            minHeight: 28,
            padding: '4px 8px 4px 0',
            paddingInlineStart: depth * 18,
            borderLeft:
              depth > 0 ? '1px solid var(--hlp-border, #2a2f38)' : '1px solid transparent',
            marginInlineStart: depth > 0 ? 9 : 0,
          }}
        >
          <button
            type="button"
            onClick={() => toggle(node.subtaskId)}
            aria-label={isCollapsed ? `展开 ${node.subtaskId}` : `折叠 ${node.subtaskId}`}
            disabled={children.length === 0}
            style={{
              width: 16,
              height: 16,
              padding: 0,
              border: 'none',
              background: 'transparent',
              color: 'var(--hlp-text-secondary, #9aa3ae)',
              cursor: children.length > 0 ? 'pointer' : 'default',
              fontSize: 10,
              lineHeight: '16px',
            }}
          >
            {children.length > 0 ? (isCollapsed ? '▸' : '▾') : '·'}
          </button>

          <Tag
            style={{
              marginInlineEnd: 0,
              fontSize: 10,
              lineHeight: '16px',
              paddingInline: 6,
              background: 'transparent',
              borderColor: 'var(--hlp-border, #2a2f38)',
              color: 'var(--hlp-text-secondary, #9aa3ae)',
            }}
          >
            {node.role}
          </Tag>

          <span
            style={{
              flex: 1,
              minWidth: 0,
              overflow: 'hidden',
              textOverflow: 'ellipsis',
              whiteSpace: 'nowrap',
              fontSize: 12,
              color: 'var(--hlp-text, #e6e9ef)',
            }}
            title={node.title || node.subtaskId}
          >
            {node.title || node.subtaskId}
          </span>

          {node.failureReason && (
            <span
              style={{
                fontSize: 11,
                color: 'var(--hlp-danger, #e5534b)',
                maxWidth: 240,
                overflow: 'hidden',
                textOverflow: 'ellipsis',
                whiteSpace: 'nowrap',
              }}
              title={node.failureReason}
            >
              {node.failureReason}
            </span>
          )}

          <Tag
            color={STATUS_COLORS[node.status]}
            style={{ marginInlineEnd: 0, fontSize: 10, lineHeight: '16px' }}
          >
            <span
              aria-hidden="true"
              style={{
                display: 'inline-block',
                width: 6,
                height: 6,
                borderRadius: '50%',
                marginInlineEnd: 5,
                background: STATUS_DOTS[node.status],
              }}
            />
            {SUBTASK_STATUS_LABELS[node.status]}
          </Tag>

          <span
            style={{
              fontSize: 11,
              color: 'var(--hlp-text-secondary, #9aa3ae)',
              fontVariantNumeric: 'tabular-nums',
              minWidth: 46,
            }}
          >
            attempt {node.attempt}
          </span>

          <span
            style={{
              fontSize: 11,
              color: 'var(--hlp-text-secondary, #9aa3ae)',
              fontVariantNumeric: 'tabular-nums',
              minWidth: 48,
              textAlign: 'right',
            }}
          >
            {formatDuration(elapsed)}
          </span>
        </div>

        {!isCollapsed && children.length > 0 && (
          <div>{children.map((child) => renderNode(child))}</div>
        )}
      </div>
    );
  };

  return <div>{tree.map((item) => renderNode(item))}</div>;
}