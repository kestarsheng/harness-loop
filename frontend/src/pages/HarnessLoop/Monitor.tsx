import { useCallback, useEffect, useMemo, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { Alert, Button, Segmented, Space, Tag, Tooltip, Typography } from 'antd';
import {
  ApiOutlined,
  DisconnectOutlined,
  PartitionOutlined,
  ReloadOutlined,
  SyncOutlined,
} from '@ant-design/icons';
import harnessLoopApi from './api/harnessLoop';
import { useEventStream } from './hooks/useEventStream';
import type { StreamStatus } from './hooks/useEventStream';
import AgentCardGrid from './components/AgentCardGrid';
import AnomalyDrawer from './components/AnomalyDrawer';
import ControlBar from './components/ControlBar';
import type { ControlAction } from './components/ControlBar';
import HealthGauge from './components/HealthGauge';
import LifecycleSwimlane from './components/LifecycleSwimlane';
import StageDurationChart from './components/StageDurationChart';
import SummaryBar from './components/SummaryBar';
import { useMonitorStore } from './store';
import type { StatusFilter } from './store';
import type { HarnessEvent, HarnessTaskView, TaskRuntimeState } from './types';
import {
  hydrateTaskState,
  reduceMonitorState,
  statusLabel,
} from './types';

const FILTER_OPTIONS: Array<{ label: string; value: StatusFilter }> = [
  { label: '全部', value: 'all' },
  { label: '运行中', value: 'running' },
  { label: '等待验收', value: 'waiting_acceptance' },
  { label: '重试中', value: 'retrying' },
  { label: '失败', value: 'failed' },
  { label: '完成', value: 'completed' },
];

const STREAM_BADGES: Record<StreamStatus, { color: string; text: string }> = {
  connecting: { color: 'var(--hlp-warn, #e8b339)', text: '实时连接中' },
  open: { color: 'var(--hlp-primary, #2bd4a8)', text: '实时已连接' },
  reconnecting: { color: 'var(--hlp-warn, #e8b339)', text: '断线重连中' },
  degraded: { color: 'var(--hlp-danger, #e5534b)', text: '实时不可用 · 5s 轮询' },
  closed: { color: 'var(--hlp-muted, #6b7280)', text: '实时未连接' },
};

function mergeTaskViews(
  previous: Record<string, TaskRuntimeState>,
  views: HarnessTaskView[],
): Record<string, TaskRuntimeState> {
  let next = previous;
  for (const view of views) {
    const hydrated = hydrateTaskState(view);
    const current = next[view.task_id];
    if (!current) {
      next = { ...next, [view.task_id]: hydrated };
      continue;
    }
    if (hydrated.updatedAtMs <= current.updatedAtMs) {
      continue;
    }
    next = {
      ...next,
      [view.task_id]: {
        ...current,
        status: hydrated.status,
        stage: hydrated.stage ?? current.stage,
        attemptNoByStage: hydrated.attemptNoByStage,
        retryCount: Math.max(current.retryCount, hydrated.retryCount),
        acceptanceFailed: Math.max(current.acceptanceFailed, hydrated.acceptanceFailed),
        terminalReason: hydrated.terminalReason || current.terminalReason,
        lastCheckpointId: hydrated.lastCheckpointId || current.lastCheckpointId,
        updatedAtMs: hydrated.updatedAtMs,
      },
    };
  }
  return next;
}

export default function Monitor() {
  const navigate = useNavigate();
  const [eventStates, setEventStates] = useState<Record<string, TaskRuntimeState>>({});
  const [now, setNow] = useState(() => Date.now());
  const [streamStatus, setStreamStatus] = useState<StreamStatus>('closed');
  const [streamError, setStreamError] = useState<string | null>(null);
  const [operating, setOperating] = useState(false);

  const selectedTaskId = useMonitorStore((state) => state.selectedTaskId);
  const anomalyTaskId = useMonitorStore((state) => state.anomalyTaskId);
  const anomalyTraceId = useMonitorStore((state) => state.anomalyTraceId);
  const statusFilter = useMonitorStore((state) => state.statusFilter);
  const selectTask = useMonitorStore((state) => state.selectTask);
  const openAnomaly = useMonitorStore((state) => state.openAnomaly);
  const closeAnomaly = useMonitorStore((state) => state.closeAnomaly);
  const setStatusFilter = useMonitorStore((state) => state.setStatusFilter);

  const { data, isLoading, isError, refetch } = useQuery({
    queryKey: ['hlp', 'tasks'],
    queryFn: async () => {
      const response = await harnessLoopApi.listTasks({ limit: 50 });
      return (response?.tasks ?? []) as HarnessTaskView[];
    },
    refetchInterval: streamStatus === 'degraded' ? 5000 : 10000,
    staleTime: 5000,
  });

  const states = useMemo(
    () => (data ? mergeTaskViews(eventStates, data) : eventStates),
    [eventStates, data],
  );

  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, []);

  const getLastSeq = useCallback(() => {
    let maxSeq = 0;
    for (const state of Object.values(eventStates)) {
      maxSeq = Math.max(maxSeq, state.lastEventSeq);
    }
    return maxSeq;
  }, [eventStates]);

  const handleEvents = useCallback((events: HarnessEvent[]) => {
    setEventStates((previous) => reduceMonitorState(previous, events));
  }, []);

  const handleStreamStatus = useCallback((status: StreamStatus) => {
    setStreamStatus(status);
    if (status === 'open') {
      setStreamError(null);
    }
  }, []);

  const handleStreamError = useCallback((message: string) => {
    setStreamError(message);
  }, []);

  useEventStream({
    tenantId:
      (typeof localStorage !== 'undefined' && localStorage.getItem('tenantId')) || 'default',
    enabled: true,
    getLastSeq,
    onEvents: handleEvents,
    onStatus: handleStreamStatus,
    onError: handleStreamError,
  });

  const visibleStates = useMemo(() => {
    const list = Object.values(states);
    const filtered =
      statusFilter === 'all'
        ? list
        : list.filter((state) => state.status === statusFilter);
    return filtered.sort((left, right) => {
      const leftActive = left.status === 'running' || left.status === 'retrying' ? 1 : 0;
      const rightActive = right.status === 'running' || right.status === 'retrying' ? 1 : 0;
      if (leftActive !== rightActive) {
        return rightActive - leftActive;
      }
      return right.updatedAtMs - left.updatedAtMs;
    });
  }, [states, statusFilter]);

  const selectedState = selectedTaskId ? states[selectedTaskId] : undefined;

  const aggregateHealth = useMemo(() => {
    if (visibleStates.length === 0) {
      return { score: 0, sampleInsufficient: true };
    }
    const total = visibleStates.reduce((sum, state) => sum + state.healthScore, 0);
    const withSamples = visibleStates.some((state) => state.acceptanceTotal > 0);
    return {
      score: Math.round(total / visibleStates.length),
      sampleInsufficient: !withSamples,
    };
  }, [visibleStates]);

  const handleAction = useCallback(
    async (action: ControlAction) => {
      if (!selectedTaskId) {
        return;
      }
      setOperating(true);
      try {
        if (action === 'pause') {
          await harnessLoopApi.pauseTask(selectedTaskId, 'monitor');
        } else if (action === 'resume') {
          await harnessLoopApi.resumeTask(selectedTaskId);
        } else {
          await harnessLoopApi.abortTask(selectedTaskId, 'monitor');
        }
        await refetch();
      } finally {
        setOperating(false);
      }
    },
    [selectedTaskId, refetch],
  );

  const badge = STREAM_BADGES[streamStatus];

  return (
    <div
      style={{
        margin: -24,
        padding: 20,
        minHeight: 'calc(100% + 48px)',
        background: 'var(--hlp-bg, #0f1115)',
        color: 'var(--hlp-text, #e6e9ef)',
        boxSizing: 'border-box',
      }}
    >
      <div
        style={{
          display: 'flex',
          alignItems: 'center',
          gap: 12,
          marginBottom: 16,
          flexWrap: 'wrap',
        }}
      >
        <div style={{ fontSize: 18, fontWeight: 600 }}>任务循环监控</div>
        <Tag
          style={{
            marginInlineEnd: 0,
            background: 'transparent',
            borderColor: badge.color,
            color: badge.color,
            fontSize: 11,
          }}
        >
          {streamStatus === 'open' ? <ApiOutlined /> : streamStatus === 'degraded' ? <DisconnectOutlined /> : <SyncOutlined spin />}
          <span style={{ marginInlineStart: 6 }}>{badge.text}</span>
        </Tag>
        <div style={{ flex: 1 }} />
        <Segmented
          size="small"
          value={statusFilter}
          options={FILTER_OPTIONS}
          onChange={(value) => setStatusFilter(value as StatusFilter)}
        />
        <Tooltip title="立即刷新任务快照">
          <Button size="small" icon={<ReloadOutlined />} onClick={() => void refetch()} loading={isLoading}>
            刷新
          </Button>
        </Tooltip>
      </div>

      {streamStatus === 'degraded' && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 12 }}
          message="实时通道不可用，已退化为 5s 轮询；断线补齐以数据库快照为准。"
        />
      )}

      {streamError && (
        <Alert
          type="error"
          showIcon
          closable
          style={{ marginBottom: 12 }}
          message="实时通道错误"
          description={streamError}
          onClose={() => setStreamError(null)}
        />
      )}

      {isError && (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 12 }}
          message="任务列表拉取失败，请检查 Gateway / harness-loop 服务是否可用。"
        />
      )}

      <SummaryBar states={visibleStates} />

      <div
        style={{
          display: 'grid',
          gridTemplateColumns: 'minmax(0, 1fr) 320px',
          gap: 12,
          marginTop: 12,
        }}
      >
        <div
          style={{
            background: 'var(--hlp-panel, #161a20)',
            border: '1px solid var(--hlp-border, #2a2f38)',
            borderRadius: 'var(--hlp-radius, 10px)',
            padding: 12,
            height: 'calc(100vh - 380px)',
            minHeight: 320,
            boxSizing: 'border-box',
          }}
        >
          <div
            style={{
              fontSize: 12,
              color: 'var(--hlp-text-secondary, #9aa3ae)',
              marginBottom: 8,
            }}
          >
            任务卡片（{visibleStates.length}）· 虚拟滚动
          </div>
          <AgentCardGrid
            states={visibleStates}
            now={now}
            selectedTaskId={selectedTaskId}
            onSelect={selectTask}
            onOpenAnomaly={openAnomaly}
            emptyPlaceholder={
              <div style={{ textAlign: 'left', maxWidth: 520 }}>
                <div style={{ marginBottom: 8, color: 'var(--hlp-text, #e6e9ef)', fontSize: 14 }}>
                  暂无任务。创建任务后再回到此页查看阶段推进。
                </div>
                <Typography.Paragraph
                  style={{ marginBottom: 6, color: 'var(--hlp-text-secondary, #9aa3ae)', fontSize: 12 }}
                >
                  CLI：
                </Typography.Paragraph>
                <pre
                  style={{
                    background: 'var(--hlp-panel-strong, #1c2129)',
                    border: '1px solid var(--hlp-border, #2a2f38)',
                    borderRadius: 6,
                    padding: '8px 10px',
                    fontSize: 12,
                    overflowX: 'auto',
                    color: 'var(--hlp-text, #e6e9ef)',
                  }}
                >
{`harness-cli run --goal "补全登录模块单测" \\
  --workspace ./repo --project proj-a`}
                </pre>
                <Typography.Paragraph
                  style={{ marginBottom: 6, color: 'var(--hlp-text-secondary, #9aa3ae)', fontSize: 12 }}
                >
                  API：
                </Typography.Paragraph>
                <pre
                  style={{
                    background: 'var(--hlp-panel-strong, #1c2129)',
                    border: '1px solid var(--hlp-border, #2a2f38)',
                    borderRadius: 6,
                    padding: '8px 10px',
                    fontSize: 12,
                    overflowX: 'auto',
                    color: 'var(--hlp-text, #e6e9ef)',
                  }}
                >
{`curl -X POST /api/v2/harness-loop/tasks \\
  -H 'X-Tenant-ID: default' \\
  -d '{"goal":"...","workspace_root":"...","project_id":"..."}'`}
                </pre>
              </div>
            }
          />
        </div>

        <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
          <div
            style={{
              background: 'var(--hlp-panel, #161a20)',
              border: '1px solid var(--hlp-border, #2a2f38)',
              borderRadius: 'var(--hlp-radius, 10px)',
              padding: 12,
            }}
          >
            <div style={{ fontSize: 12, color: 'var(--hlp-text-secondary, #9aa3ae)' }}>
              平均健康度
            </div>
            <HealthGauge
              score={aggregateHealth.score}
              sampleInsufficient={aggregateHealth.sampleInsufficient}
            />
            {selectedState && (
              <div style={{ fontSize: 11, color: 'var(--hlp-muted, #6b7280)', marginTop: 4 }}>
                选中 {selectedState.taskId.slice(0, 12)} · {statusLabel(selectedState.status)}
              </div>
            )}
          </div>

          <div
            style={{
              background: 'var(--hlp-panel, #161a20)',
              border: '1px solid var(--hlp-border, #2a2f38)',
              borderRadius: 'var(--hlp-radius, 10px)',
              padding: 12,
            }}
          >
            <div style={{ fontSize: 12, color: 'var(--hlp-text-secondary, #9aa3ae)' }}>
              阶段平均时长
            </div>
            <StageDurationChart states={visibleStates} height={190} />
          </div>

          <ControlBar
            state={selectedState}
            operating={operating}
            onAction={handleAction}
          />

          <Button
            block
            icon={<PartitionOutlined />}
            disabled={!selectedTaskId}
            onClick={() => {
              if (selectedTaskId) {
                navigate(`/harness-loop/tasks/${encodeURIComponent(selectedTaskId)}`);
              }
            }}
          >
            子任务 / DAG 详情
          </Button>
        </div>
      </div>

      <div
        style={{
          marginTop: 12,
          background: 'var(--hlp-panel, #161a20)',
          border: '1px solid var(--hlp-border, #2a2f38)',
          borderRadius: 'var(--hlp-radius, 10px)',
          padding: 12,
        }}
      >
        <div style={{ fontSize: 12, color: 'var(--hlp-text-secondary, #9aa3ae)', marginBottom: 8 }}>
          生命周期泳道 · a→b→c→d→e · 色块 = attempt · 红点 = 回退
        </div>
        <LifecycleSwimlane states={visibleStates} height={230} />
      </div>

      <AnomalyDrawer
        open={anomalyTaskId !== null}
        taskId={anomalyTaskId}
        traceId={anomalyTraceId}
        state={anomalyTaskId ? states[anomalyTaskId] : undefined}
        onClose={closeAnomaly}
      />

      <Space style={{ marginTop: 12 }}>
        <span style={{ fontSize: 11, color: 'var(--hlp-muted, #6b7280)' }}>
          默认只读视图；暂停 / 中止需二次确认并写入审计。
        </span>
      </Space>
    </div>
  );
}