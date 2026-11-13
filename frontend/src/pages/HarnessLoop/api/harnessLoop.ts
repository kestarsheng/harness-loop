import { isAxiosError } from 'axios';
import type { AxiosRequestConfig } from 'axios';
import client from '../../../api/client';
import type { HarnessEvent, HarnessTaskView } from '../types';

const BASE = '/api/v2/harness-loop';

// ===== 后端视图类型 =====
// 字段名与 proto/harnessloop/harnessloop.proto 的 JSON 序列化保持一致（snake_case）。
// proto bytes 字段（input_contract 等）经 encoding/json 输出为 base64 字符串。

export interface SubtaskView {
  subtask_id: string;
  parent_task_id: string;
  tenant_id: string;
  project_id: string;
  dag_id: string;
  dag_node_id: string;
  depth: number;
  // proto3 默认值（0 / false / 空串）在 JSON 序列化时被省略，因此数值字段按可选处理。
  index_in_parent?: number;
  parallel_set_id?: string;
  role: string;
  status: string;
  depends_on?: string[];
  input_contract?: string | null;
  output_contract?: string | null;
  acceptance_criteria?: string | null;
  acceptance_id?: string;
  failure_strategy?: string;
  attempt_no?: number;
  artifact_refs?: string | null;
  handoff_id?: string;
  alloc_tokens?: number | string;
  consumed_tokens?: number | string;
  consumed_cost?: number | string;
  created_at_ms?: number | string;
  updated_at_ms?: number | string;
}

export interface ModelSelectionView {
  selection_id: string;
  tenant_id: string;
  project_id: string;
  task_id: string;
  stage: string;
  attempt_no?: number;
  model_id: string;
  strategy: string;
  resolved_from: string;
  reason?: string;
  candidates?: string | null;
  context_window?: number;
  threshold_tokens?: number | string;
  input_price?: number | string;
  output_price?: number | string;
  tokens_in?: number | string;
  tokens_out?: number | string;
  cost?: number | string;
  // proto3 在 false 时省略该字段。
  is_fallback?: boolean;
  approval_id?: string;
  operator?: string;
  created_at_ms?: number | string;
}

export interface HandoffView {
  handoff_id: string;
  parent_task_id?: string;
  subtask_id?: string;
  tenant_id?: string;
  project_id?: string;
  from_agent_id?: string;
  to_agent_id?: string;
  status?: string;
  reason?: string;
  input_digest?: string;
  created_at_ms?: number | string;
  updated_at_ms?: number | string;
}

export interface ListTasksParams {
  project_id?: string;
  status?: string[];
  limit?: number;
}

export interface ListEventsParams {
  task_id?: string;
  stage?: string;
  event_type?: string;
  severity?: string;
  since_seq?: number;
  limit?: number;
}

export interface ListCheckpointsParams {
  task_id: string;
  limit?: number;
}

export interface ListSubtasksParams {
  project_id?: string;
  limit?: number;
}

export interface ListModelSelectionsParams {
  stage?: string;
  project_id?: string;
  limit?: number;
}

export interface ListHandoffsParams {
  task_id?: string;
  project_id?: string;
  limit?: number;
}

export interface TaskListResponse {
  tasks: HarnessTaskView[];
}

export interface EventListResponse {
  events: HarnessEvent[];
  next_since_seq?: number | string;
}

export interface CheckpointListResponse {
  checkpoints: unknown[];
}

export interface SubtaskListResponse {
  subtasks: SubtaskView[];
}

export interface ModelSelectionListResponse {
  selections: ModelSelectionView[];
}

export interface HandoffListResponse {
  handoffs: HandoffView[];
}

// ===== 错误态 =====
// listSubtasks / listModelSelections / listHandoffs 的失败要能被页面区分：
// 401 未授权、404 任务不存在、超时、网络不可达与其它 HTTP 错误。

export type HarnessApiErrorKind =
  | 'unauthorized'
  | 'forbidden'
  | 'not_found'
  | 'timeout'
  | 'network'
  | 'http';

export interface HarnessApiErrorOptions {
  status?: number;
  code?: string;
  cause?: unknown;
}

export class HarnessApiError extends Error {
  readonly kind: HarnessApiErrorKind;
  readonly status: number;
  readonly code: string;

  constructor(kind: HarnessApiErrorKind, message: string, options: HarnessApiErrorOptions = {}) {
    super(message);
    this.name = 'HarnessApiError';
    this.kind = kind;
    this.status = options.status ?? 0;
    this.code = options.code ?? '';
    if (options.cause !== undefined) {
      (this as Error & { cause?: unknown }).cause = options.cause;
    }
  }
}

function upstreamMessage(data: unknown): string {
  if (data && typeof data === 'object' && 'message' in data) {
    const message = (data as { message?: unknown }).message;
    if (typeof message === 'string' && message.trim() !== '') {
      return message.trim();
    }
  }
  return '';
}

function upstreamCode(data: unknown): string {
  if (data && typeof data === 'object' && 'code' in data) {
    const code = (data as { code?: unknown }).code;
    if (typeof code === 'string') {
      return code;
    }
  }
  return '';
}

export function normalizeHarnessApiError(error: unknown): HarnessApiError {
  if (error instanceof HarnessApiError) {
    return error;
  }
  if (isAxiosError(error)) {
    const status = error.response?.status ?? 0;
    const data = error.response?.data;
    const message = upstreamMessage(data);
    const code = upstreamCode(data) || error.code || '';
    if (status === 401) {
      return new HarnessApiError(
        'unauthorized',
        message || '未授权：登录态失效或缺少租户身份',
        { status, code, cause: error },
      );
    }
    if (status === 403) {
      return new HarnessApiError('forbidden', message || '无权访问该任务数据', {
        status,
        code,
        cause: error,
      });
    }
    if (status === 404) {
      return new HarnessApiError('not_found', message || '任务或数据不存在', {
        status,
        code,
        cause: error,
      });
    }
    const timedOut =
      status === 408 ||
      status === 504 ||
      code === 'ECONNABORTED' ||
      code === 'ETIMEDOUT' ||
      /timeout/i.test(error.message);
    if (timedOut) {
      return new HarnessApiError('timeout', message || '请求超时，Gateway 未在时限内响应', {
        status,
        code,
        cause: error,
      });
    }
    if (!error.response) {
      return new HarnessApiError('network', message || '无法连接 Gateway，请确认服务已启动', {
        status,
        code,
        cause: error,
      });
    }
    return new HarnessApiError(
      'http',
      message || error.message || `请求失败（HTTP ${status || 'unknown'}）`,
      { status, code, cause: error },
    );
  }
  if (error instanceof Error) {
    return new HarnessApiError('http', error.message, { cause: error });
  }
  return new HarnessApiError('http', '未知请求错误');
}

function request<T>(promise: Promise<T>): Promise<T> {
  return promise.catch((error: unknown) => {
    throw normalizeHarnessApiError(error);
  });
}

const arrayAwareParams = {
  indexes: null as null,
};

// api/client.ts 的响应拦截器已经把 envelope 解包成 data，
// 这里按解包后的形状声明返回类型。
function getData<T>(url: string, config?: AxiosRequestConfig): Promise<T> {
  return client.get(url, config) as unknown as Promise<T>;
}

export const harnessLoopApi = {
  listTasks: (params: ListTasksParams = {}) =>
    request(
      getData<TaskListResponse>(`${BASE}/tasks`, {
        params: {
          project_id: params.project_id,
          status: params.status,
          limit: params.limit ?? 50,
        },
        paramsSerializer: arrayAwareParams,
      }),
    ),

  getTask: (taskId: string) =>
    request(getData<HarnessTaskView>(`${BASE}/tasks/${encodeURIComponent(taskId)}`)),

  listEvents: (params: ListEventsParams = {}) =>
    request(
      getData<EventListResponse>(`${BASE}/events`, {
        params: {
          task_id: params.task_id,
          stage: params.stage,
          event_type: params.event_type,
          severity: params.severity,
          since_seq: params.since_seq ?? 0,
          limit: params.limit ?? 200,
        },
      }),
    ),

  listCheckpoints: (params: ListCheckpointsParams) =>
    request(
      getData<CheckpointListResponse>(`${BASE}/checkpoints`, {
        params: { task_id: params.task_id, limit: params.limit ?? 20 },
      }),
    ),

  listSubtasks: (taskId: string, params: ListSubtasksParams = {}) =>
    request(
      getData<SubtaskListResponse>(
        `${BASE}/tasks/${encodeURIComponent(taskId)}/subtasks`,
        {
          params: {
            project_id: params.project_id,
            limit: params.limit ?? 200,
          },
        },
      ),
    ),

  listModelSelections: (taskId: string, params: ListModelSelectionsParams = {}) =>
    request(
      getData<ModelSelectionListResponse>(
        `${BASE}/tasks/${encodeURIComponent(taskId)}/model-selections`,
        {
          params: {
            stage: params.stage,
            project_id: params.project_id,
            limit: params.limit ?? 200,
          },
        },
      ),
    ),

  listHandoffs: (params: ListHandoffsParams = {}) =>
    request(
      getData<HandoffListResponse>(`${BASE}/handoffs`, {
        params: {
          task_id: params.task_id,
          project_id: params.project_id,
          limit: params.limit ?? 200,
        },
      }),
    ),

  pauseTask: (taskId: string, reason?: string) =>
    request(
      client.post(`${BASE}/tasks/${encodeURIComponent(taskId)}/pause`, {
        confirm: true,
        reason,
      }),
    ),

  resumeTask: (taskId: string, checkpointId?: string) =>
    request(
      client.post(`${BASE}/tasks/${encodeURIComponent(taskId)}/resume`, {
        checkpoint_id: checkpointId,
      }),
    ),

  abortTask: (taskId: string, reason?: string) =>
    request(
      client.post(`${BASE}/tasks/${encodeURIComponent(taskId)}/abort`, {
        confirm: true,
        reason,
      }),
    ),

  // SSE 协议参数名必须与 Gateway 一致：tenant / last_seq。
  streamUrl: (tenantId: string, lastSeq: number) => {
    const params = new URLSearchParams();
    params.set('tenant', tenantId || 'default');
    params.set('last_seq', String(lastSeq));
    return `${BASE}/monitor/stream?${params.toString()}`;
  },
};

export default harnessLoopApi;