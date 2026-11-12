import { useMemo } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Alert, Descriptions, Drawer, Empty, Spin, Table, Tabs, Tag } from 'antd';
import harnessLoopApi from '../api/harnessLoop';
import type { CheckpointView, HarnessEvent, TaskRuntimeState } from '../types';
import { formatDuration, parsePayload, statusLabel, toNumber } from '../types';

const SEVERITY_COLORS: Record<string, string> = {
  debug: 'default',
  info: 'blue',
  warn: 'orange',
  error: 'red',
};

export interface AnomalyDrawerProps {
  open: boolean;
  taskId: string | null;
  traceId?: string | null;
  state?: TaskRuntimeState;
  onClose: () => void;
}

function messageOf(event: HarnessEvent): string {
  const payload = parsePayload(event.payload);
  const reason = payload.reason ?? payload.message ?? payload.terminal_reason;
  if (typeof reason === 'string' && reason !== '') {
    return reason;
  }
  return event.event_type;
}

export default function AnomalyDrawer({
  open,
  taskId,
  traceId,
  state,
  onClose,
}: AnomalyDrawerProps) {
  const eventsQuery = useQuery({
    queryKey: ['hlp', 'events', taskId],
    queryFn: async () => {
      const response = await harnessLoopApi.listEvents({ task_id: taskId ?? '', limit: 200 });
      return (response?.events ?? []) as HarnessEvent[];
    },
    enabled: open && taskId !== null,
    staleTime: 2000,
  });

  const checkpointsQuery = useQuery({
    queryKey: ['hlp', 'checkpoints', taskId],
    queryFn: async () => {
      const response = await harnessLoopApi.listCheckpoints({
        task_id: taskId ?? '',
        limit: 20,
      });
      return (response?.checkpoints ?? []) as CheckpointView[];
    },
    enabled: open && taskId !== null,
    staleTime: 5000,
  });

  const events = useMemo(() => eventsQuery.data ?? [], [eventsQuery.data]);
  const checkpoints = useMemo(
    () => checkpointsQuery.data ?? [],
    [checkpointsQuery.data],
  );
  const loading = eventsQuery.isLoading;
  const error = eventsQuery.isError ? '事件拉取失败，可稍后重试' : null;

  const scopedEvents = useMemo(() => {
    if (!traceId) {
      return events;
    }
    const matched = events.filter((event) => event.trace_id === traceId);
    return matched.length > 0 ? matched : events.filter((event) => event.task_id === taskId);
  }, [events, traceId, taskId]);

  const orderedEvents = useMemo(
    () => [...scopedEvents].sort((left, right) => toNumber(left.seq) - toNumber(right.seq)),
    [scopedEvents],
  );

  const acceptanceEvents = useMemo(
    () => orderedEvents.filter((event) => event.event_type.startsWith('acceptance.')),
    [orderedEvents],
  );

  const logEvents = useMemo(
    () =>
      orderedEvents.filter(
        (event) =>
          event.event_type === 'error' ||
          event.event_type === 'security.violation' ||
          event.severity === 'error' ||
          event.severity === 'warn',
      ),
    [orderedEvents],
  );

  const renderEventTable = (rows: HarnessEvent[]) => (
    <Table<HarnessEvent>
      size="small"
      rowKey="event_id"
      pagination={false}
      scroll={{ y: 'calc(100vh - 320px)' }}
      dataSource={rows}
      columns={[
        {
          title: 'seq',
          dataIndex: 'seq',
          width: 70,
          render: (value: number | string) => <span style={{ fontVariantNumeric: 'tabular-nums' }}>{String(value)}</span>,
        },
        {
          title: '时间',
          dataIndex: 'timestamp_ms',
          width: 150,
          render: (value: number | string) => new Date(toNumber(value)).toLocaleString(),
        },
        { title: '类型', dataIndex: 'event_type', width: 180 },
        {
          title: '阶段',
          dataIndex: 'stage',
          width: 70,
          render: (value: string | null | undefined) => (value ? value.toUpperCase() : '—'),
        },
        {
          title: '级别',
          dataIndex: 'severity',
          width: 80,
          render: (value: string) => <Tag color={SEVERITY_COLORS[value] ?? 'default'}>{value}</Tag>,
        },
        {
          title: '摘要',
          key: 'message',
          render: (_value, record) => messageOf(record),
        },
      ]}
    />
  );

  return (
    <Drawer
      title={`异常定位 · ${taskId ?? ''}`}
      width={860}
      open={open}
      onClose={onClose}
      destroyOnClose
    >
      {state && (
        <Descriptions size="small" column={3} style={{ marginBottom: 16 }}>
          <Descriptions.Item label="状态">{statusLabel(state.status)}</Descriptions.Item>
          <Descriptions.Item label="阶段">
            {state.stage ? state.stage.toUpperCase() : '—'}
          </Descriptions.Item>
          <Descriptions.Item label="健康度">{state.healthScore}</Descriptions.Item>
          <Descriptions.Item label="trace_id" span={2}>
            <span style={{ fontFamily: 'ui-monospace, monospace', fontSize: 12 }}>
              {traceId || state.anomalies[0]?.traceId || '—'}
            </span>
          </Descriptions.Item>
          <Descriptions.Item label="最近检查点" span={3}>
            <span style={{ fontFamily: 'ui-monospace, monospace', fontSize: 12 }}>
              {state.lastCheckpointId || '—'}
            </span>
          </Descriptions.Item>
        </Descriptions>
      )}

      {error && <Alert type="warning" showIcon message={error} style={{ marginBottom: 12 }} />}

      {loading ? (
        <Spin tip="加载 trace 关联数据">
          <div style={{ height: 180 }} />
        </Spin>
      ) : (
        <Tabs
          items={[
            {
              key: 'events',
              label: `事件（${orderedEvents.length}）`,
              children:
                orderedEvents.length > 0 ? (
                  renderEventTable(orderedEvents)
                ) : (
                  <Empty description="该 trace 暂无事件" />
                ),
            },
            {
              key: 'acceptance',
              label: `验收记录（${acceptanceEvents.length}）`,
              children:
                acceptanceEvents.length > 0 ? (
                  renderEventTable(acceptanceEvents)
                ) : (
                  <Empty description="暂无验收记录" />
                ),
            },
            {
              key: 'logs',
              label: `日志摘要（${logEvents.length}）`,
              children:
                logEvents.length > 0 ? (
                  renderEventTable(logEvents)
                ) : (
                  <Empty description="暂无 warn/error 日志" />
                ),
            },
            {
              key: 'checkpoints',
              label: `检查点（${checkpoints.length}）`,
              children:
                checkpoints.length > 0 ? (
                  <Table<CheckpointView>
                    size="small"
                    rowKey="checkpoint_id"
                    pagination={false}
                    dataSource={checkpoints}
                    columns={[
                      {
                        title: '检查点',
                        dataIndex: 'checkpoint_id',
                        render: (value: string) => (
                          <span style={{ fontFamily: 'ui-monospace, monospace', fontSize: 12 }}>
                            {value}
                          </span>
                        ),
                      },
                      { title: '类型', dataIndex: 'kind', width: 100 },
                      {
                        title: '阶段',
                        dataIndex: 'stage',
                        width: 70,
                        render: (value: string) => (value ? value.toUpperCase() : '—'),
                      },
                      { title: 'attempt', dataIndex: 'attempt', width: 80 },
                      {
                        title: '水位 seq',
                        dataIndex: 'event_seq_watermark',
                        width: 110,
                        render: (value: number | string) => String(value ?? '—'),
                      },
                      {
                        title: '创建时间',
                        dataIndex: 'created_at_ms',
                        width: 160,
                        render: (value: number | string) =>
                          value ? new Date(toNumber(value)).toLocaleString() : '—',
                      },
                    ]}
                  />
                ) : (
                  <Empty description="暂无检查点" />
                ),
            },
          ]}
        />
      )}

      {state && state.anomalies.length > 0 && (
        <div style={{ marginTop: 16 }}>
          <div style={{ fontSize: 12, color: 'var(--hlp-text-secondary, #9aa3ae)', marginBottom: 8 }}>
            最近异常（{state.anomalies.length}）
          </div>
          {state.anomalies.slice(0, 5).map((anomaly) => (
            <div
              key={anomaly.eventId}
              style={{
                padding: '6px 10px',
                marginBottom: 6,
                border: '1px solid var(--hlp-border, #2a2f38)',
                borderRadius: 6,
                fontSize: 12,
                color: 'var(--hlp-text, #e6e9ef)',
              }}
            >
              <span style={{ color: 'var(--hlp-danger, #e5534b)' }}>
                {anomaly.stage ? anomaly.stage.toUpperCase() : '—'}
              </span>
              <span style={{ marginInline: 8, color: 'var(--hlp-muted, #6b7280)' }}>
                seq {anomaly.seq}
              </span>
              {anomaly.message}
              {anomaly.traceId && (
                <span
                  style={{
                    marginInlineStart: 8,
                    fontFamily: 'ui-monospace, monospace',
                    color: 'var(--hlp-text-secondary, #9aa3ae)',
                  }}
                >
                  trace {anomaly.traceId.slice(0, 12)}
                </span>
              )}
            </div>
          ))}
        </div>
      )}

      {state?.startedAtMs ? (
        <div style={{ marginTop: 12, fontSize: 12, color: 'var(--hlp-muted, #6b7280)' }}>
          运行时长 {formatDuration(state.updatedAtMs - state.startedAtMs)}
        </div>
      ) : null}
    </Drawer>
  );
}