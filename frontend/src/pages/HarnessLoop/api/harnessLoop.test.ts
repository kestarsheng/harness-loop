import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { HandoffView, ModelSelectionView, SubtaskView } from './harnessLoop';

const { getMock, postMock } = vi.hoisted(() => ({
  getMock: vi.fn(),
  postMock: vi.fn(),
}));

vi.mock('../../../api/client', () => ({
  default: {
    get: getMock,
    post: postMock,
  },
}));

const {
  HarnessApiError,
  harnessLoopApi,
  normalizeHarnessApiError,
} = await import('./harnessLoop');

function axiosError(options: {
  status?: number;
  code?: string;
  message?: string;
  data?: unknown;
  withResponse?: boolean;
}) {
  const withResponse = options.withResponse ?? options.status !== undefined;
  return {
    isAxiosError: true,
    message: options.message ?? 'Request failed',
    code: options.code,
    response: withResponse
      ? { status: options.status ?? 500, data: options.data }
      : undefined,
  };
}

const subtask: SubtaskView = {
  subtask_id: 'st-a-1',
  parent_task_id: 'task-a2',
  tenant_id: 'tenant-a',
  project_id: 'proj-a',
  dag_id: 'dag-1',
  dag_node_id: 'node-a1',
  depth: 1,
  index_in_parent: 0,
  parallel_set_id: 'ps-a',
  role: 'coder',
  status: 'running',
  attempt_no: 1,
  created_at_ms: 1_760_000_000_000,
  updated_at_ms: 1_760_000_001_000,
};

const selection: ModelSelectionView = {
  selection_id: 'sel-c-1',
  tenant_id: 'tenant-a',
  project_id: 'proj-a',
  task_id: 'task-a2',
  stage: 'c',
  attempt_no: 2,
  model_id: 'qwen-max',
  strategy: 'balanced',
  resolved_from: 'fallback',
  reason: 'primary unavailable',
  tokens_in: 200,
  tokens_out: 80,
  cost: 0.0012,
  is_fallback: true,
  created_at_ms: 1_760_000_002_000,
};

const handoff: HandoffView = {
  handoff_id: 'ho-1',
  parent_task_id: 'task-a2',
  subtask_id: 'st-a-1',
  tenant_id: 'tenant-a',
  project_id: 'proj-a',
  from_agent_id: 'agent-a',
  to_agent_id: 'agent-b',
  status: 'accepted',
  input_digest: 'sha256:abc',
  created_at_ms: 1_760_000_000_000,
  updated_at_ms: 1_760_000_003_000,
};

beforeEach(() => {
  getMock.mockReset();
  postMock.mockReset();
});

describe('harnessLoopApi 真实 GET', () => {
  it('listSubtasks 按后端路径与 query 参数请求，并保留 snake_case 字段', async () => {
    getMock.mockResolvedValue({ subtasks: [subtask] });

    const response = await harnessLoopApi.listSubtasks('task/a2', {
      project_id: 'proj-a',
      limit: 10,
    });

    expect(getMock).toHaveBeenCalledTimes(1);
    expect(getMock).toHaveBeenCalledWith(
      '/api/v2/harness-loop/tasks/task%2Fa2/subtasks',
      {
        params: {
          project_id: 'proj-a',
          limit: 10,
        },
      },
    );
    expect(response.subtasks[0].subtask_id).toBe('st-a-1');
    expect(response.subtasks[0].parallel_set_id).toBe('ps-a');
    expect(response.subtasks[0].attempt_no).toBe(1);
  });

  it('listSubtasks 默认 limit=200，project_id 为空', async () => {
    getMock.mockResolvedValue({ subtasks: [] });

    await harnessLoopApi.listSubtasks('task-a2');

    expect(getMock).toHaveBeenCalledWith(
      '/api/v2/harness-loop/tasks/task-a2/subtasks',
      { params: { project_id: undefined, limit: 200 } },
    );
  });

  it('listModelSelections 透传 stage / project_id / limit', async () => {
    getMock.mockResolvedValue({ selections: [selection] });

    const response = await harnessLoopApi.listModelSelections('task-a2', {
      stage: 'c',
      project_id: 'proj-a',
      limit: 5,
    });

    expect(getMock).toHaveBeenCalledWith(
      '/api/v2/harness-loop/tasks/task-a2/model-selections',
      {
        params: {
          stage: 'c',
          project_id: 'proj-a',
          limit: 5,
        },
      },
    );
    expect(response.selections[0].model_id).toBe('qwen-max');
    expect(response.selections[0].is_fallback).toBe(true);
    expect(response.selections[0].tokens_in).toBe(200);
  });

  it('listHandoffs 使用 task_id 过滤，不使用 role 字段', async () => {
    getMock.mockResolvedValue({ handoffs: [handoff] });

    const response = await harnessLoopApi.listHandoffs({
      task_id: 'task-a2',
      project_id: 'proj-a',
    });

    expect(getMock).toHaveBeenCalledWith('/api/v2/harness-loop/handoffs', {
      params: {
        task_id: 'task-a2',
        project_id: 'proj-a',
        limit: 200,
      },
    });
    expect(response.handoffs[0].from_agent_id).toBe('agent-a');
    expect(response.handoffs[0].to_agent_id).toBe('agent-b');
    expect('from_role' in response.handoffs[0]).toBe(false);
    expect('to_role' in response.handoffs[0]).toBe(false);
  });

  it('streamUrl 固定使用 tenant / last_seq 协议参数', () => {
    expect(harnessLoopApi.streamUrl('tenant-a', 42)).toBe(
      '/api/v2/harness-loop/monitor/stream?tenant=tenant-a&last_seq=42',
    );
    expect(harnessLoopApi.streamUrl('', 0)).toBe(
      '/api/v2/harness-loop/monitor/stream?tenant=default&last_seq=0',
    );
  });
});

describe('normalizeHarnessApiError 错误态', () => {
  it('401 映射为 unauthorized 并保留后端 message', async () => {
    getMock.mockRejectedValue(
      axiosError({
        status: 401,
        data: { code: 'E_UNAUTHENTICATED', message: 'token expired' },
      }),
    );

    await expect(harnessLoopApi.listSubtasks('task-a2')).rejects.toMatchObject({
      name: 'HarnessApiError',
      kind: 'unauthorized',
      status: 401,
      code: 'E_UNAUTHENTICATED',
      message: 'token expired',
    });
  });

  it('404 映射为 not_found', async () => {
    getMock.mockRejectedValue(
      axiosError({
        status: 404,
        data: { code: 'E_NOT_FOUND', message: 'task not found' },
      }),
    );

    await expect(
      harnessLoopApi.listModelSelections('task-a2'),
    ).rejects.toMatchObject({
      kind: 'not_found',
      status: 404,
      code: 'E_NOT_FOUND',
    });
  });

  it('超时（ECONNABORTED 且无响应）映射为 timeout', async () => {
    getMock.mockRejectedValue(
      axiosError({
        code: 'ECONNABORTED',
        message: 'timeout of 300000ms exceeded',
        withResponse: false,
      }),
    );

    await expect(harnessLoopApi.listHandoffs({ task_id: 'task-a2' })).rejects.toMatchObject({
      kind: 'timeout',
      status: 0,
      message: '请求超时，Gateway 未在时限内响应',
    });
  });

  it('HTTP 504 也映射为 timeout', async () => {
    getMock.mockRejectedValue(axiosError({ status: 504 }));

    await expect(harnessLoopApi.listSubtasks('task-a2')).rejects.toMatchObject({
      kind: 'timeout',
      status: 504,
    });
  });

  it('无响应且非超时映射为 network', async () => {
    getMock.mockRejectedValue(
      axiosError({ code: 'ERR_NETWORK', message: 'Network Error', withResponse: false }),
    );

    await expect(harnessLoopApi.listSubtasks('task-a2')).rejects.toMatchObject({
      kind: 'network',
    });
  });

  it('403 映射为 forbidden，其它 5xx 映射为 http', async () => {
    getMock.mockRejectedValueOnce(axiosError({ status: 403 }));
    await expect(harnessLoopApi.listHandoffs()).rejects.toMatchObject({ kind: 'forbidden' });

    getMock.mockRejectedValueOnce(axiosError({ status: 500, message: 'boom' }));
    await expect(harnessLoopApi.listHandoffs()).rejects.toMatchObject({
      kind: 'http',
      status: 500,
      message: 'boom',
    });
  });

  it('已归一化的错误不会被二次包装', () => {
    const original = new HarnessApiError('timeout', 'custom timeout');
    expect(normalizeHarnessApiError(original)).toBe(original);
  });

  it('普通 Error 归一化为 http', () => {
    const result = normalizeHarnessApiError(new Error('plain failure'));
    expect(result.kind).toBe('http');
    expect(result.message).toBe('plain failure');
  });
});