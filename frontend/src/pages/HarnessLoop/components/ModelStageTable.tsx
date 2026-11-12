import { useMemo } from 'react';
import { Table, Tag } from 'antd';
import type { ModelStageUsage, ModelUsageState } from '../modelUsage';
import { modelStageRows } from '../modelUsage';
import { STAGE_LABELS } from '../types';
import type { StageCode } from '../types';

export interface ModelStageTableProps {
  state: ModelUsageState;
}

function formatCost(value: number): string {
  if (!Number.isFinite(value) || value <= 0) {
    return '—';
  }
  return `$${value.toFixed(3)}`;
}

export default function ModelStageTable({ state }: ModelStageTableProps) {
  const rows = useMemo(() => modelStageRows(state), [state]);

  return (
    <Table<ModelStageUsage>
      size="small"
      rowKey={(record) => record.stage}
      pagination={false}
      dataSource={rows}
      locale={{ emptyText: '暂无模型选择记录' }}
      columns={[
        {
          title: '阶段',
          dataIndex: 'stage',
          width: 118,
          render: (stage: StageCode) => (
            <span>
              <span style={{ fontFamily: 'ui-monospace, monospace', fontSize: 12 }}>
                {stage.toUpperCase()}
              </span>
              <span
                style={{
                  marginInlineStart: 6,
                  fontSize: 11,
                  color: 'var(--hlp-text-secondary, #9aa3ae)',
                }}
              >
                {STAGE_LABELS[stage]}
              </span>
            </span>
          ),
        },
        {
          title: '生效模型',
          dataIndex: 'model',
          width: 170,
          render: (model: string, record) => {
            if (!model) {
              return <span style={{ color: 'var(--hlp-muted, #6b7280)' }}>—</span>;
            }
            return (
              <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
                <span
                  style={{
                    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
                    fontSize: 12,
                    color: 'var(--hlp-text, #e6e9ef)',
                  }}
                >
                  {model}
                </span>
                {record.fallbackCount > 0 && (
                  <Tag color="warning" style={{ marginInlineEnd: 0, fontSize: 10, lineHeight: '16px' }}>
                    回退 {record.fallbackCount}
                  </Tag>
                )}
              </span>
            );
          },
        },
        {
          title: '选择次数',
          dataIndex: 'selections',
          width: 80,
          align: 'right',
          render: (value: number) => (value > 0 ? value : '—'),
        },
        {
          title: '回退次数',
          dataIndex: 'fallbackCount',
          width: 80,
          align: 'right',
          render: (value: number) =>
            value > 0 ? (
              <span style={{ color: 'var(--hlp-warn, #e8b339)' }}>{value}</span>
            ) : (
              '0'
            ),
        },
        {
          title: 'token',
          dataIndex: 'tokens',
          width: 92,
          align: 'right',
          render: (value: number) => (value > 0 ? value.toLocaleString() : '—'),
        },
        {
          title: '成本',
          dataIndex: 'cost',
          width: 78,
          align: 'right',
          render: (value: number) => formatCost(value),
        },
      ]}
    />
  );
}