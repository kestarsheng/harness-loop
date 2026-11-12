import { useMemo, useState } from 'react';
import type { CSSProperties } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import { Alert, Button, Collapse, Space, Tag } from 'antd';
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
import {
  buildMockHandoffs,
  buildMockModelUsage,
  buildMockPolicyDenyState,
  buildMockSubtaskSnapshot,
  buildMockTaskSummary,
} from './mockData';
import { computeDagProgress } from './subtask';
import { STAGE_LABELS, formatDuration } from './types';
import type { StageCode } from './types';

const panelStyle: CSSProperties = {
  background: 'var(--hlp-panel, #161a20)',
  border: '1px solid var(--hlp-border, #2a2f38)',
  borderRadius: 'var(--hlp-radius, 10px)',
  padding: 12,
};

export default function TaskDetail() {
  const { taskId = 'task-demo-parent' } = useParams<{ taskId: string }>();
  const navigate = useNavigate();
  const [nowMs] = useState(() => Date.now());
  const [denyOpen, setDenyOpen] = useState(false);

  const summary = useMemo(() => buildMockTaskSummary(taskId, nowMs), [taskId, nowMs]);
  const subtaskSnapshot = useMemo(
    () => buildMockSubtaskSnapshot(taskId, nowMs),
    [taskId, nowMs],
  );
  const modelUsage = useMemo(() => buildMockModelUsage(nowMs), [nowMs]);
  const policyDenyState = useMemo(() => buildMockPolicyDenyState(nowMs), [nowMs]);
  const handoffs = useMemo(() => buildMockHandoffs(nowMs), [nowMs]);
  const dagProgress = useMemo(() => computeDagProgress(subtaskSnapshot), [subtaskSnapshot]);

  const elapsed = nowMs - summary.startedAtMs;

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
          任务详情 · <span style={{ fontFamily: 'ui-monospace, monospace', fontSize: 15 }}>{taskId}</span>
        </div>
        <div style={{ flex: 1 }} />
        <Button
          size="small"
          icon={<SafetyOutlined />}
          onClick={() => setDenyOpen(true)}
        >
          权限拒绝（{policyDenyState.total}）
        </Button>
      </div>

      <Alert
        type="warning"
        showIcon
        style={{ marginBottom: 12 }}
        message="未联调：子任务、模型选择与 handoff 数据源尚未在 Gateway 暴露"
        description="本页由 mock 数据驱动，仅用于验证组件与归约逻辑；父任务展开、DAG 真实进度与模型分布不能据此认定后端已可用。"
      />

      <div style={{ ...panelStyle, marginBottom: 12 }}>
        <div style={{ fontSize: 13, marginBottom: 6 }}>{summary.goal}</div>
        <Space size={12} wrap style={{ fontSize: 12 }}>
          <span style={{ color: 'var(--hlp-text-secondary, #9aa3ae)' }}>
            状态{' '}
            <Tag color="processing" style={{ marginInlineStart: 4 }}>
              {summary.status === 'running' ? '运行中' : summary.status}
            </Tag>
          </span>
          <span style={{ color: 'var(--hlp-text-secondary, #9aa3ae)' }}>
            当前阶段{' '}
            <span style={{ color: 'var(--hlp-text, #e6e9ef)' }}>
              {summary.stage.toUpperCase()}{' '}
              {STAGE_LABELS[summary.stage as StageCode] ?? ''}
            </span>
          </span>
          <span style={{ color: 'var(--hlp-text-secondary, #9aa3ae)' }}>
            运行时长{' '}
            <span style={{ color: 'var(--hlp-text, #e6e9ef)' }}>{formatDuration(elapsed)}</span>
          </span>
          <span style={{ color: 'var(--hlp-text-secondary, #9aa3ae)' }}>
            子任务{' '}
            <span style={{ color: 'var(--hlp-text, #e6e9ef)' }}>{dagProgress.total}</span>
          </span>
        </Space>
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
              子任务树 · 最多 2 层缩进 · 可折叠
            </div>
            <SubagentTree snapshot={subtaskSnapshot} nowMs={nowMs} />
          </div>

          <Collapse
            size="small"
            defaultActiveKey={[]}
            items={[
              {
                key: 'dag',
                label: `DAG 进度与并行集时间线 · ${dagProgress.counts.accepted}/${dagProgress.total} 已验收`,
                children: (
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
            <ModelStageTable state={modelUsage} />
            <div
              style={{
                marginTop: 8,
                fontSize: 11,
                color: 'var(--hlp-muted, #6b7280)',
              }}
            >
              配置版本 {modelUsage.configVersion || '—'} · 卡片仅显示模型短名与回退角标
            </div>
          </div>

          <div style={panelStyle}>
            <ModelDistributionChart state={modelUsage} nowMs={nowMs} />
          </div>

          <div style={panelStyle}>
            <div
              style={{
                fontSize: 12,
                color: 'var(--hlp-text-secondary, #9aa3ae)',
                marginBottom: 8,
              }}
            >
              handoff 契约图
            </div>
            <HandoffGraph handoffs={handoffs} />
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