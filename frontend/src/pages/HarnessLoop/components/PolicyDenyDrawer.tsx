import { Drawer, Empty, Table, Tag } from 'antd';
import type { PolicyDenyItem, PolicyDenyState } from '../policyDeny';
import { policyDenyDistribution } from '../policyDeny';

export interface PolicyDenyDrawerProps {
  open: boolean;
  taskId?: string | null;
  state: PolicyDenyState;
  onClose: () => void;
}

const LAYER_LABELS: Record<string, string> = {
  sandbox: '沙箱',
  permission: '权限',
  security: '安全',
  unknown: '未分类',
};

function formatTimestamp(value: number): string {
  if (!Number.isFinite(value) || value <= 0) {
    return '—';
  }
  return new Date(value).toLocaleString();
}

export default function PolicyDenyDrawer({
  open,
  taskId,
  state,
  onClose,
}: PolicyDenyDrawerProps) {
  const distribution = policyDenyDistribution(state);

  return (
    <Drawer
      title={`权限拒绝明细 · ${taskId ?? ''}`}
      width={780}
      open={open}
      onClose={onClose}
      destroyOnClose
    >
      <div
        style={{
          display: 'flex',
          alignItems: 'center',
          gap: 8,
          flexWrap: 'wrap',
          marginBottom: 12,
          fontSize: 12,
          color: 'var(--hlp-text-secondary, #9aa3ae)',
        }}
      >
        <span>
          拒绝合计 <span style={{ color: 'var(--hlp-text, #e6e9ef)' }}>{state.total}</span> 次
        </span>
        {Object.entries(state.byLayer).map(([layer, count]) => (
          <Tag key={layer} style={{ marginInlineEnd: 0, fontSize: 11 }}>
            {LAYER_LABELS[layer] ?? layer} {count}
          </Tag>
        ))}
      </div>

      {distribution.length > 0 && (
        <div style={{ marginBottom: 12 }}>
          <div
            style={{ fontSize: 11, color: 'var(--hlp-text-secondary, #9aa3ae)', marginBottom: 6 }}
          >
            按规则聚合
          </div>
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
            {distribution.map((entry) => (
              <Tag
                key={`${entry.layer}:${entry.ruleId}`}
                color="default"
                style={{ marginInlineEnd: 0, fontSize: 11 }}
                title={`layer: ${entry.layer}`}
              >
                <span style={{ fontFamily: 'ui-monospace, monospace' }}>{entry.ruleId}</span>
                <span style={{ marginInlineStart: 6 }}>{entry.count}</span>
              </Tag>
            ))}
          </div>
        </div>
      )}

      {state.items.length === 0 ? (
        <Empty description="暂无权限拒绝记录" image={Empty.PRESENTED_IMAGE_SIMPLE} />
      ) : (
        <Table<PolicyDenyItem>
          size="small"
          rowKey="eventId"
          pagination={{ pageSize: 12, size: 'small' }}
          dataSource={state.items}
          expandable={{
            expandedRowRender: (record) => (
              <div
                style={{
                  display: 'grid',
                  gridTemplateColumns: '120px minmax(0, 1fr)',
                  rowGap: 6,
                  columnGap: 10,
                  fontSize: 12,
                  color: 'var(--hlp-text, #e6e9ef)',
                }}
              >
                <span style={{ color: 'var(--hlp-text-secondary, #9aa3ae)' }}>rule_id</span>
                <span style={{ fontFamily: 'ui-monospace, monospace' }}>{record.ruleId}</span>
                <span style={{ color: 'var(--hlp-text-secondary, #9aa3ae)' }}>layer</span>
                <span>
                  {record.layer}（{LAYER_LABELS[record.layer] ?? '未分类'}）
                </span>
                <span style={{ color: 'var(--hlp-text-secondary, #9aa3ae)' }}>拒绝原因</span>
                <span>{record.reason}</span>
                <span style={{ color: 'var(--hlp-text-secondary, #9aa3ae)' }}>参数摘要</span>
                <span
                  style={{
                    fontFamily: 'ui-monospace, monospace',
                    wordBreak: 'break-all',
                    color: 'var(--hlp-text, #e6e9ef)',
                  }}
                >
                  {record.argSummary}
                </span>
                <span style={{ color: 'var(--hlp-text-secondary, #9aa3ae)' }}>来源事件</span>
                <span style={{ fontFamily: 'ui-monospace, monospace', fontSize: 11 }}>
                  {record.source} · seq {record.seq} · {record.eventId}
                </span>
              </div>
            ),
          }}
          columns={[
            {
              title: '时间',
              dataIndex: 'timestampMs',
              width: 150,
              render: (value: number) => formatTimestamp(value),
            },
            {
              title: '拒绝',
              key: 'deny',
              render: (_value, record) => (
                <span>
                  <span style={{ fontFamily: 'ui-monospace, monospace' }}>{record.tool}</span>
                  <span style={{ marginInlineStart: 6 }}>被拒</span>
                  <span
                    style={{
                      marginInlineStart: 6,
                      color: 'var(--hlp-text-secondary, #9aa3ae)',
                      fontFamily: 'ui-monospace, monospace',
                      fontSize: 11,
                    }}
                  >
                    ({record.ruleId})
                  </span>
                </span>
              ),
            },
            {
              title: '层',
              dataIndex: 'layer',
              width: 92,
              render: (value: string) => (
                <Tag style={{ marginInlineEnd: 0, fontSize: 10, lineHeight: '16px' }}>
                  {LAYER_LABELS[value] ?? value}
                </Tag>
              ),
            },
            {
              title: '阶段',
              dataIndex: 'stage',
              width: 64,
              render: (value: string | null) => (value ? value.toUpperCase() : '—'),
            },
            {
              title: '原因',
              dataIndex: 'reason',
              ellipsis: true,
            },
          ]}
        />
      )}

      <div
        style={{
          marginTop: 14,
          fontSize: 11,
          color: 'var(--hlp-muted, #6b7280)',
          lineHeight: 1.6,
        }}
      >
        拒绝是硬约束：此处只展示规则与参数摘要，不提供“忽略拒绝继续”的入口。
        参数中的凭据字段已在归约阶段脱敏，详情不含原始对话。
      </div>
    </Drawer>
  );
}