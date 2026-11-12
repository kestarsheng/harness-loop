import type { HarnessEvent } from './types';
import type { HandoffContractSummary, SubtaskSnapshot } from './subtask';
import { createSubtaskSnapshot, reduceSubtaskState } from './subtask';
import type { ModelUsageState } from './modelUsage';
import { createModelUsageState, reduceModelUsage } from './modelUsage';
import type { PolicyDenyState } from './policyDeny';
import { createPolicyDenyState, reducePolicyDenies } from './policyDeny';

const MINUTE = 60 * 1000;

export interface MockTaskSummary {
  taskId: string;
  goal: string;
  status: string;
  stage: string;
  startedAtMs: number;
}

let mockSeq = 0;

function nextSeq(): number {
  mockSeq += 1;
  return mockSeq;
}

interface MockEventInput {
  type: string;
  taskId: string;
  timestampMs: number;
  payload?: Record<string, unknown>;
  stage?: string;
  subtaskId?: string;
  dagId?: string;
  parentTaskId?: string;
  severity?: string;
}

function mockEvent(input: MockEventInput): HarnessEvent {
  return {
    event_id: `mock-${input.type}-${nextSeq()}`,
    event_type: input.type,
    task_id: input.taskId,
    tenant_id: 'tenant-demo',
    project_id: 'proj-demo',
    seq: nextSeq(),
    timestamp_ms: input.timestampMs,
    severity: input.severity ?? 'info',
    stage: input.stage ?? null,
    subtask_id: input.subtaskId,
    dag_id: input.dagId,
    parent_task_id: input.parentTaskId,
    payload: JSON.stringify(input.payload ?? {}),
  };
}

export function buildMockTaskSummary(taskId: string, nowMs = Date.now()): MockTaskSummary {
  return {
    taskId,
    goal: '补齐登录模块的错误态与接口契约测试',
    status: 'running',
    stage: 'c',
    startedAtMs: nowMs - 42 * MINUTE,
  };
}

export function buildMockSubtaskSnapshot(
  taskId: string,
  nowMs = Date.now(),
): SubtaskSnapshot {
  mockSeq = 0;
  const dagId = 'dag-demo-login';
  const events: HarnessEvent[] = [
    mockEvent({
      type: 'subtask.queued',
      taskId,
      parentTaskId: taskId,
      subtaskId: 'st-plan',
      dagId,
      timestampMs: nowMs - 41 * MINUTE,
      payload: { role: 'planner', title: '拆解登录模块改造范围', parallel_set_id: 'ps-plan' },
    }),
    mockEvent({
      type: 'subtask.started',
      taskId,
      parentTaskId: taskId,
      subtaskId: 'st-plan',
      dagId,
      timestampMs: nowMs - 40 * MINUTE,
      payload: { role: 'planner', attempt: 1, parallel_set_id: 'ps-plan' },
    }),
    mockEvent({
      type: 'subtask.ended',
      taskId,
      parentTaskId: taskId,
      subtaskId: 'st-plan',
      dagId,
      timestampMs: nowMs - 33 * MINUTE,
      payload: { status: 'accepted', attempt: 1 },
    }),
    mockEvent({
      type: 'subtask.queued',
      taskId,
      parentTaskId: taskId,
      subtaskId: 'st-api',
      dagId,
      timestampMs: nowMs - 33 * MINUTE,
      payload: {
        role: 'backend',
        title: '登录接口与错误码改造',
        parent_subtask_id: 'st-plan',
        parallel_set_id: 'ps-impl',
      },
    }),
    mockEvent({
      type: 'subtask.queued',
      taskId,
      parentTaskId: taskId,
      subtaskId: 'st-ui',
      dagId,
      timestampMs: nowMs - 33 * MINUTE,
      payload: {
        role: 'frontend',
        title: '登录页错误态与表单校验',
        parent_subtask_id: 'st-plan',
        parallel_set_id: 'ps-impl',
      },
    }),
    mockEvent({
      type: 'subtask.queued',
      taskId,
      parentTaskId: taskId,
      subtaskId: 'st-docs',
      dagId,
      timestampMs: nowMs - 32 * MINUTE,
      payload: {
        role: 'writer',
        title: '登录改造说明与迁移指引',
        parent_subtask_id: 'st-plan',
        parallel_set_id: 'ps-impl',
      },
    }),
    mockEvent({
      type: 'subtask.started',
      taskId,
      parentTaskId: taskId,
      subtaskId: 'st-api',
      dagId,
      timestampMs: nowMs - 31 * MINUTE,
      payload: { role: 'backend', attempt: 1, parallel_set_id: 'ps-impl' },
    }),
    mockEvent({
      type: 'subtask.ended',
      taskId,
      parentTaskId: taskId,
      subtaskId: 'st-api',
      dagId,
      timestampMs: nowMs - 24 * MINUTE,
      payload: {
        status: 'failed',
        attempt: 1,
        reason: '契约测试未通过：错误码 401 与 403 混用',
      },
    }),
    mockEvent({
      type: 'subtask.started',
      taskId,
      parentTaskId: taskId,
      subtaskId: 'st-api',
      dagId,
      timestampMs: nowMs - 22 * MINUTE,
      payload: { role: 'backend', attempt: 2, parallel_set_id: 'ps-impl' },
    }),
    mockEvent({
      type: 'subtask.queued',
      taskId,
      parentTaskId: taskId,
      subtaskId: 'st-api-unit',
      dagId,
      timestampMs: nowMs - 21 * MINUTE,
      payload: {
        role: 'backend',
        title: '接口单元测试',
        parent_subtask_id: 'st-api',
        parallel_set_id: 'ps-api-leaf',
      },
    }),
    mockEvent({
      type: 'subtask.started',
      taskId,
      parentTaskId: taskId,
      subtaskId: 'st-api-unit',
      dagId,
      timestampMs: nowMs - 20 * MINUTE,
      payload: { role: 'backend', attempt: 1, parallel_set_id: 'ps-api-leaf' },
    }),
    mockEvent({
      type: 'subtask.ended',
      taskId,
      parentTaskId: taskId,
      subtaskId: 'st-api-unit',
      dagId,
      timestampMs: nowMs - 15 * MINUTE,
      payload: { status: 'accepted', attempt: 1 },
    }),
    mockEvent({
      type: 'subtask.started',
      taskId,
      parentTaskId: taskId,
      subtaskId: 'st-ui',
      dagId,
      timestampMs: nowMs - 30 * MINUTE,
      payload: { role: 'frontend', attempt: 1, parallel_set_id: 'ps-impl' },
    }),
    mockEvent({
      type: 'subtask.ended',
      taskId,
      parentTaskId: taskId,
      subtaskId: 'st-ui',
      dagId,
      timestampMs: nowMs - 14 * MINUTE,
      payload: { status: 'failed', attempt: 1, reason: '验收未通过：缺少提交失败态提示' },
    }),
    mockEvent({
      type: 'subtask.queued',
      taskId,
      parentTaskId: taskId,
      subtaskId: 'st-ui-fix',
      dagId,
      timestampMs: nowMs - 12 * MINUTE,
      payload: {
        role: 'frontend',
        title: '补充提交失败态提示',
        parent_subtask_id: 'st-ui',
        parallel_set_id: 'ps-ui-leaf',
      },
    }),
    mockEvent({
      type: 'subtask.started',
      taskId,
      parentTaskId: taskId,
      subtaskId: 'st-ui-fix',
      dagId,
      timestampMs: nowMs - 10 * MINUTE,
      payload: { role: 'frontend', attempt: 1, parallel_set_id: 'ps-ui-leaf' },
    }),
    mockEvent({
      type: 'subtask.started',
      taskId,
      parentTaskId: taskId,
      subtaskId: 'st-docs',
      dagId,
      timestampMs: nowMs - 29 * MINUTE,
      payload: { role: 'writer', attempt: 1, parallel_set_id: 'ps-impl' },
    }),
    mockEvent({
      type: 'subtask.ended',
      taskId,
      parentTaskId: taskId,
      subtaskId: 'st-docs',
      dagId,
      timestampMs: nowMs - 18 * MINUTE,
      payload: { status: 'cancelled', attempt: 1, reason: '父任务范围收敛，文档延后到 e 阶段' },
    }),
    mockEvent({
      type: 'subtask.queued',
      taskId,
      parentTaskId: taskId,
      subtaskId: 'st-api-e2e',
      dagId,
      timestampMs: nowMs - 8 * MINUTE,
      payload: {
        role: 'qa',
        title: '登录全链路回归',
        parent_subtask_id: 'st-api',
        parallel_set_id: 'ps-api-leaf',
      },
    }),
  ];

  const initial = createSubtaskSnapshot(taskId, dagId);
  return events.reduce<SubtaskSnapshot>(
    (snapshot, event) => reduceSubtaskState(snapshot, event),
    initial,
  );
}

export function buildMockModelUsage(nowMs = Date.now()): ModelUsageState {
  mockSeq = 0;
  const taskId = 'task-demo-parent';
  const events: HarnessEvent[] = [
    mockEvent({
      type: 'model.config_applied',
      taskId,
      timestampMs: nowMs - 45 * MINUTE,
      payload: { version: '2026.11.12-r3' },
    }),
    mockEvent({
      type: 'model.selected',
      taskId,
      stage: 'a',
      timestampMs: nowMs - 40 * MINUTE,
      payload: { model: 'qwen-max', tokens: 1200, cost: 0.012 },
    }),
    mockEvent({
      type: 'model.selected',
      taskId,
      stage: 'b',
      timestampMs: nowMs - 34 * MINUTE,
      payload: { model: 'qwen-plus', tokens: 3400, cost: 0.021 },
    }),
    mockEvent({
      type: 'model.fallback',
      taskId,
      stage: 'b',
      timestampMs: nowMs - 32 * MINUTE,
      payload: { from: 'qwen-plus', to: 'qwen-max', trigger: 'timeout' },
    }),
    mockEvent({
      type: 'model.selected',
      taskId,
      stage: 'b',
      timestampMs: nowMs - 31 * MINUTE,
      payload: { model: 'qwen-max', tokens: 2200, cost: 0.03 },
    }),
    mockEvent({
      type: 'model.selected',
      taskId,
      stage: 'c',
      timestampMs: nowMs - 28 * MINUTE,
      payload: { model: 'qwen-plus', tokens: 8200, cost: 0.062 },
    }),
    mockEvent({
      type: 'model.fallback',
      taskId,
      stage: 'c',
      timestampMs: nowMs - 16 * MINUTE,
      payload: { from: 'qwen-plus', to: 'qwen-max', trigger: 'rate_limit' },
    }),
    mockEvent({
      type: 'model.selected',
      taskId,
      stage: 'c',
      timestampMs: nowMs - 15 * MINUTE,
      payload: { model: 'qwen-max', tokens: 5600, cost: 0.048 },
    }),
    mockEvent({
      type: 'model.selected',
      taskId,
      stage: 'd',
      timestampMs: nowMs - 10 * MINUTE,
      payload: { model: 'qwen-max', tokens: 2100, cost: 0.019 },
    }),
    mockEvent({
      type: 'model.selected',
      taskId,
      stage: 'e',
      timestampMs: nowMs - 2 * MINUTE,
      payload: { model: 'qwen-turbo', tokens: 900, cost: 0.008 },
    }),
  ];

  return events.reduce<ModelUsageState>(
    (state, event) => reduceModelUsage(state, event),
    createModelUsageState(),
  );
}

export function buildMockPolicyDenyState(nowMs = Date.now()): PolicyDenyState {
  mockSeq = 0;
  const taskId = 'task-demo-parent';
  const events: HarnessEvent[] = [
    mockEvent({
      type: 'tool.denied',
      taskId,
      stage: 'c',
      timestampMs: nowMs - 26 * MINUTE,
      payload: {
        tool: 'write_file',
        rule_id: 'sandbox.write_allowlist',
        layer: 'sandbox',
        reason: '写入路径不在白名单',
        args: { path: '/etc/hosts', content_length: 128 },
      },
    }),
    mockEvent({
      type: 'tool.denied',
      taskId,
      stage: 'c',
      timestampMs: nowMs - 23 * MINUTE,
      payload: {
        tool: 'shell_exec',
        rule_id: 'sandbox.command_allowlist',
        layer: 'sandbox',
        reason: '命令不在白名单',
        args: { cmd: 'curl http://example.internal', timeout: 30 },
      },
    }),
    mockEvent({
      type: 'security.violation',
      taskId,
      stage: 'c',
      timestampMs: nowMs - 20 * MINUTE,
      severity: 'warn',
      payload: {
        action: 'git_push',
        rule_id: 'permission.deny',
        layer: 'permission',
        reason: '禁止直接推送受保护分支',
        args: { branch: 'main', token: 'sk-demo-value' },
      },
    }),
    mockEvent({
      type: 'tool.denied',
      taskId,
      stage: 'c',
      timestampMs: nowMs - 17 * MINUTE,
      payload: {
        tool: 'read_file',
        rule_id: 'workspace.path_scope',
        layer: 'sandbox',
        reason: '读取越出工作目录',
        args: { path: '~/.ssh/id_rsa' },
      },
    }),
    mockEvent({
      type: 'tool.denied',
      taskId,
      stage: 'c',
      timestampMs: nowMs - 9 * MINUTE,
      payload: {
        tool: 'write_file',
        rule_id: 'sandbox.write_allowlist',
        layer: 'sandbox',
        reason: '写入路径不在白名单',
        args: { path: '../outside/repo.txt' },
      },
    }),
  ];

  return events.reduce<PolicyDenyState>(
    (state, event) => reducePolicyDenies(state, event),
    createPolicyDenyState(),
  );
}

export function buildMockHandoffs(nowMs = Date.now()): HandoffContractSummary[] {
  return [
    {
      handoffId: 'ho-plan-api',
      fromRole: 'planner',
      toRole: 'backend',
      scope: '接口签名与错误码契约',
      artifactCount: 3,
      acceptedCriteria: ['接口签名冻结', '错误码表补齐 401/403 区分'],
      createdAtMs: nowMs - 33 * MINUTE,
    },
    {
      handoffId: 'ho-plan-ui',
      fromRole: 'planner',
      toRole: 'frontend',
      scope: '交互状态与组件 props 契约',
      artifactCount: 4,
      acceptedCriteria: ['空/错/载入三态覆盖', 'props 命名冻结'],
      createdAtMs: nowMs - 32 * MINUTE,
    },
    {
      handoffId: 'ho-api-qa',
      fromRole: 'backend',
      toRole: 'qa',
      scope: '可测端点与测试数据',
      artifactCount: 2,
      acceptedCriteria: ['契约测试可执行', '全链路用例可直接复用'],
      createdAtMs: nowMs - 15 * MINUTE,
    },
    {
      handoffId: 'ho-ui-writer',
      fromRole: 'frontend',
      toRole: 'writer',
      scope: '页面状态与对外文案清单',
      artifactCount: 2,
      acceptedCriteria: ['文案与状态一致', '不包含内部实现细节'],
      createdAtMs: nowMs - 12 * MINUTE,
    },
  ];
}