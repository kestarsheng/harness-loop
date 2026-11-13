import { useEffect, useMemo, useState } from 'react';
import type { CSSProperties, ReactNode } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import type { UseQueryResult } from '@tanstack/react-query';
import { Alert, Button, Collapse, Result, Skeleton, Space, Tag } from 'antd';
import {
  ArrowLeftOutlined,
  PartitionOutlined,
  SafetyOutlined,
} from '@ant-design/icons';
import DagProgressBar from './components/DagProgressBar';
import DagTimeline from './components/DagTimeline';
import HandoffGraph from './components/HandoffGraph';
import ModelDistributionChart from './components/ModelDistributionChart';
import ModelStageTable from './components/ModelStageTable';
import PolicyDenyDrawer from './components/PolicyDenyDrawer';
import SubagentTree from './components/SubagentTree';
import harnessLoopApi, { normalizeHarnessApiError } from './api/harnessLoop';
import { toHandoffRows, toModelUsageState, toSubtaskSnapshot } from './adapters';
import { createPolicyDenyState, reducePolicyDenies } from './policyDeny';
import { computeDagProgress } from './subtask';
import {
  STAGE_LABELS,
  formatDuration,
  normalizeStage,
  statusLabel,
  toNumber,
} from './types';
import type { StageCode, TaskStatus } from './types';

const panelStyle: CSSProperties = {
  background: 'var(--hlp-panel, #161a20)',
  border: '1px solid var(--hlp-border, #2a2f38)',
  borderRadius: 'var(--hlp-radius, 10px)',
  padding: 12,
};

const pageStyle: CSSProperties = {
  margin: -24,
  padding: 20,
  minHeight: 'calc(100% + 48px)',
  background: 'var(--hlp-bg, #0f1115)',
  color: 'var(--hlp-text, #e6e9ef)',
  boxSizing: 'border-box',
};

const STATUS_TAG_COLORS: Record<string, string> = {
  pending: 'default',
  running: 'processing',
  waiting_acceptance: 'warning',
  retrying: 'warning',
  paused: 'default',
  completed: 'success',
  failed: 'error',
  aborted: 'default',
};

function stateTitle(error: unknown): { title: string; description: string; status: '404' | 'error' | '403' } {
  const apiError = normalizeHarnessApiError(error);
  switch (apiError.kind) {
    case 'unauthorized':
      return {
        title: '未授权',
        description: '登录态失效或缺少租户身份，请重新登录后重试。',
        status: '403',
      };
    case 'forbidden':
      return {
        title: '无权访问',
        description: '当前租户没有该任务的访问权限。',
        status: '403',
      };
    case 'not_found':
      return {
        title: '任务不存在',
        description: '该任务在当前租户 / 项目下不存在，或已被清理。',
        status: '404',
      };
    case 'timeout':
      return {
        title: '请求超时',
        description: 'Gateway 或 harness-loop 服务未在时限内响应，请稍后重试。',
        status: 'error',
      };
    case 'network':
      return {
        title: '无法连接 Gateway',
        description: '请确认 Gateway 已启动且前端代理地址可达。',
        status: 'error',
      };
    default:
      return {
        title: '加载失败',
        description: apiError.message || '请求失败，请稍后重试。',
        status: 'error',
      };
  }
}

function QueryFallback({
  query,
  children,
  rows = 4,
}: {
  query: UseQueryResult;
  children: ReactNode;
  rows?: number;
}) {
  if (query.isLoading) {
    return <Skeleton active paragraph={{ rows }} title={false} />;
  }
  if (query.isError) {
    const state = stateTitle(query.error);
    return (
      <Alert
        type="error"
        showIcon
        message={state.title}
        description={state.description}
        action={
          <Button size="small" onClick={() => void query.refetch()}>
            重试
          </Button>
        }
      />
    );
  }
  return <>{children}</>;
}

export default function TaskDetail() {
  const { taskId = '' } = useParams<{ taskId: string }>();
  const navigate = useNavigate();
  const [nowMs, setNowMs] = useState(() => Date.now());
  const [denyOpen, setDenyOpen] = useState(false);

  useEffect(() => {
    const timer = setInterval(() => setNowMs(Date.now()), 1000);
    return () => clearInterval(timer);
  }, []);

  const taskQuery = useQuery({
    queryKey: ['hlp', 'task', taskId],
    queryFn: () => harnessLoopApi.getTask(taskId),
    staleTime: 5000,
    enabled: taskId !== '',
  });

  const projectId = taskQuery.data?.project_id || undefined;

  const subtaskQuery = useQuery({
    queryKey: ['hlp', 'subtasks', taskId, projectId ?? ''],
    queryFn: () => harnessLoopApi.listSubtasks(taskId, { project_id: projectId }),
    staleTime: 5000,
    enabled: taskId !== '',
  });

  const modelQuery = useQuery({
    queryKey: ['hlp', 'model-selections', taskId, projectId ?? ''],
    queryFn: () => harnessLoopApi.listModelSelections(taskId, { project_id: projectId }),
    staleTime: 5000,
    enabled: taskId !== '',
  });

  const handoffQuery = useQuery({
    queryKey: ['hlp', 'handoffs', taskId, projectId ?? ''],
    queryFn: () => harnessLoopApi.listHandoffs({ task_id: taskId, project_id: projectId }),
    staleTime: 5000,
    enabled: taskId !== '',
  });

  const eventsQuery = useQuery({
    queryKey: ['hlp', 'task-events', taskId],
    queryFn: () => harnessLoopApi.listEvents({ task_id: taskId, limit: 500 }),
    staleTime: 5000,
    enabled: taskId !== '',
  });

  const subtaskSnapshot = useMemo(
    () => toSubtaskSnapshot(subtaskQuery.data?.subtasks ?? [], taskId),
    [subtaskQuery.data, taskId],
  );
  const dagProgress = useMemo(() => computeDagProgress(subtaskSnapshot), [subtaskSnapshot]);
  const modelUsage = useMemo(
    () => toModelUsageState(modelQuery.data?.selections ?? []),
    [modelQuery.data],
  );
  const handoffs = useMemo(
    () => toHandoffRows(handoffQuery.data?.handoffs ?? []),
    [handoffQuery.data],
  );
  const policyDenyState = useMemo(() => {
    const events = [...(eventsQuery.data?.events ?? [])].sort(
      (left, right) => toNumber(left.seq) - toNumber(right.seq),
    );
    return events.reduce(
      (state, event) => reducePolicyDenies(state, event),
      createPolicyDenyState(),
    );
  }, [eventsQuery.data]);

  const task = taskQuery.data;

  if (taskQuery.isLoading) {
    return (
      <div style={pageStyle}>
        <Skeleton active paragraph={{ rows: 8 }} />
      </div>
    );
  }

  if (!task) {
    const state = stateTitle(taskQuery.error);
    return (
      <div style={pageStyle}>
        <Result
          status={state.status}
          title={state.title}
          subTitle={state.description}
          extra={
            <Space>
              <Button onClick={() => navigate('/harness-loop/monitor')}>返回监控</Button>
              <Button type="primary" onClick={() => void taskQuery.refetch()}>
                重试
              </Button>
            </Space>
          }
        />
      </div>
    );
  }

  const stage = normalizeStage(task.current_stage);
  const startedAtMs = toNumber(task.created_at_ms);
  const elapsed = startedAtMs > 0 ? nowMs - startedAtMs : 0;
  const taskStatus = (task.status as TaskStatus) || 'pending';

  return (
    <div style={pageStyle}>
      <div
        style={{
          display: 'flex',
          alignItems: 'center',
          gap: 12,
          marginBottom: 12,
          flexWrap: 'wrap',
        }}
      >
        <Button
          size="small"
          icon={<ArrowLeftOutlined />}
          onClick={() => navigate('/harness-loop/monitor')}
        >
          返回监控
        </Button>
        <div style={{ fontSize: 18, fontWeight: 600 }}>
          任务详情 · <span style={{ fontFamily: 'ui-monospace, monospace', fontSize: 15 }}>{task.task_id}</span>
        </div>
        <Tag color="success" style={{ marginInlineEnd: 0 }}>
          实时数据
        </Tag>
        <div style={{ flex: 1 }} />
        <Button
          size="small"
          icon={<SafetyOutlined />}
          disabled={eventsQuery.isError}
          onClick={() => setDenyOpen(true)}
        >
          权限拒绝（{eventsQuery.isError ? '—' : policyDenyState.total}）
        </Button>
      </div>

      {eventsQuery.isError && (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 12 }}
          message="权限拒绝事件拉取失败"
          description={stateTitle(eventsQuery.error).description}
          action={
            <Button size="small" onClick={() => void eventsQuery.refetch()}>
              重试
            </Button>
          }
        />
      )}

      <div style={{ ...panelStyle, marginBottom: 12 }}>
        <div style={{ fontSize: 13, marginBottom: 6 }}>{task.goal || '—'}</div>
        <Space size={12} wrap style={{ fontSize: 12 }}>
          <span style={{ color: 'var(--hlp-text-secondary, #9aa3ae)' }}>
            状态{' '}
            <Tag
              color={STATUS_TAG_COLORS[taskStatus] ?? 'default'}
              style={{ marginInlineStart: 4 }}
            >
              {statusLabel(taskStatus)}
            </Tag>
          </span>
          <span style={{ color: 'var(--hlp-text-secondary, #9aa3ae)' }}>
            当前阶段{' '}
            <span style={{ color: 'var(--hlp-text, #e6e9ef)' }}>
              {stage ? `${stage.toUpperCase()} ${STAGE_LABELS[stage as StageCode]}` : '—'}
            </span>
          </span>
          <span style={{ color: 'var(--hlp-text-secondary, #9aa3ae)' }}>
            运行时长{' '}
            <span style={{ color: 'var(--hlp-text, #e6e9ef)' }}>
              {startedAtMs > 0 ? formatDuration(elapsed) : '—'}
            </span>
          </span>
          <span style={{ color: 'var(--hlp-text-secondary, #9aa3ae)' }}>
            子任务{' '}
            <span style={{ color: 'var(--hlp-text, #e6e9ef)' }}>{dagProgress.total}</span>
          </span>
          <span style={{ color: 'var(--hlp-text-secondary, #9aa3ae)' }}>
            项目{' '}
            <span style={{ color: 'var(--hlp-text, #e6e9ef)' }}>{task.project_id || '—'}</span>
          </span>
        </Space>
        {taskQuery.isError && (
          <Alert
            type="error"
            showIcon
            style={{ marginTop: 8 }}
            message="任务快照刷新失败"
            description={stateTitle(taskQuery.error).description}
            action={
              <Button size="small" onClick={() => void taskQuery.refetch()}>
                重试
              </Button>
            }
          />
        )}
      </div>

      <div
        style={{
          display: 'grid',
          gridTemplateColumns: 'minmax(0, 1.35fr) minmax(320px, 1fr)',
          gap: 12,
          alignItems: 'start',
        }}
      >
        <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
          <div style={panelStyle}>
            <div
              style={{
                display: 'flex',
                alignItems: 'center',
                gap: 6,
                fontSize: 12,
                color: 'var(--hlp-text-secondary, #9aa3ae)',
                marginBottom: 6,
              }}
            >
              <PartitionOutlined />
              子任务 · 来自 hlp_task.subtasks（按 DAG 顺序）
            </div>
            <QueryFallback query={subtaskQuery} rows={5}>
              <SubagentTree snapshot={subtaskSnapshot} nowMs={nowMs} />
            </QueryFallback>
          </div>

          <Collapse
            size="small"
            defaultActiveKey={[]}
            items={[
              {
                key: 'dag',
                label: `DAG 进度与并行集时间线 · ${dagProgress.counts.accepted}/${dagProgress.total} 已验收`,
                children: (
                  <QueryFallback query={subtaskQuery} rows={5}>
                    <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
                      <DagProgressBar progress={dagProgress} />
                      <div
                        style={{
                          fontSize: 11,
                          color: 'var(--hlp-text-secondary, #9aa3ae)',
                          marginTop: 4,
                        }}
                      >
                        并行集甘特（颜色深浅区分并行集；失败节点为警示色）
                      </div>
                      <DagTimeline snapshot={subtaskSnapshot} nowMs={nowMs} height={210} />
                    </div>
                  </QueryFallback>
                ),
              },
            ]}
          />
        </div>

        <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
          <div style={panelStyle}>
            <div
              style={{
                fontSize: 12,
                color: 'var(--hlp-text-secondary, #9aa3ae)',
                marginBottom: 8,
              }}
            >
              每阶段生效模型 · token / 成本
            </div>
            <QueryFallback query={modelQuery}>
              <ModelStageTable state={modelUsage} />
            </QueryFallback>
            <div
              style={{
                marginTop: 8,
                fontSize: 11,
                color: 'var(--hlp-muted, #6b7280)',
              }}
            >
              来源 hlp_task.model_selections · 卡片仅显示模型短名与回退角标
            </div>
          </div>

          <div style={panelStyle}>
            <QueryFallback query={modelQuery}>
              <ModelDistributionChart state={modelUsage} nowMs={nowMs} />
            </QueryFallback>
          </div>

          <div style={panelStyle}>
            <div
              style={{
                fontSize: 12,
                color: 'var(--hlp-text-secondary, #9aa3ae)',
                marginBottom: 8,
              }}
            >
              handoff 契约图 · 来自 handoff.* 事件聚合
            </div>
            <QueryFallback query={handoffQuery}>
              <HandoffGraph handoffs={handoffs} />
            </QueryFallback>
          </div>
        </div>
      </div>

      <PolicyDenyDrawer
        open={denyOpen}
        taskId={taskId}
        state={policyDenyState}
        onClose={() => setDenyOpen(false)}
      />
    </div>
  );
}