import type { AxiosRequestConfig } from 'axios';
import client from '../../../api/client';

const BASE = '/api/v2/harness-loop';

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

export interface TaskListResponse {
  tasks: unknown[];
}

export interface EventListResponse {
  events: unknown[];
  next_since_seq?: number | string;
}

export interface CheckpointListResponse {
  checkpoints: unknown[];
}

export interface SubtaskListResponse {
  subtasks: unknown[];
}

export interface ModelSelectionListResponse {
  selections: unknown[];
}

export interface HandoffListResponse {
  handoffs: unknown[];
}

/**
 * 本轮数据源未实现：proto 无 ListSubtasks / ListModelSelections / handoff 查询，
 * Gateway 也没有 subtasks、model-selections、handoff 路由。
 * 保留函数签名并显式抛错，避免调用方误以为拿到真实空列表。
 */
export class ApiNotImplementedError extends Error {
  readonly feature: string;

  constructor(feature: string) {
    super(`harness-loop: ${feature} 数据源未实现（未联调）`);
    this.name = 'ApiNotImplementedError';
    this.feature = feature;
  }
}

function notImplemented<T>(feature: string): Promise<T> {
  return Promise.reject(new ApiNotImplementedError(feature));
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
    getData<TaskListResponse>(`${BASE}/tasks`, {
      params: {
        project_id: params.project_id,
        status: params.status,
        limit: params.limit ?? 50,
      },
      paramsSerializer: arrayAwareParams,
    }),

  getTask: (taskId: string) => getData<unknown>(`${BASE}/tasks/${encodeURIComponent(taskId)}`),

  listEvents: (params: ListEventsParams = {}) =>
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

  listCheckpoints: (params: ListCheckpointsParams) =>
    getData<CheckpointListResponse>(`${BASE}/checkpoints`, {
      params: { task_id: params.task_id, limit: params.limit ?? 20 },
    }),

  // 预留：后端尚无对应 RPC / Gateway 路由，调用即抛 ApiNotImplementedError。
  listSubtasks: (taskId: string): Promise<SubtaskListResponse> => {
    void taskId;
    return notImplemented<SubtaskListResponse>('ListSubtasks');
  },

  listModelSelections: (taskId: string): Promise<ModelSelectionListResponse> => {
    void taskId;
    return notImplemented<ModelSelectionListResponse>('ListModelSelections');
  },

  listHandoffs: (taskId: string): Promise<HandoffListResponse> => {
    void taskId;
    return notImplemented<HandoffListResponse>('handoff 查询');
  },

  pauseTask: (taskId: string, reason?: string) =>
    client.post(`${BASE}/tasks/${encodeURIComponent(taskId)}/pause`, { confirm: true, reason }),

  resumeTask: (taskId: string, checkpointId?: string) =>
    client.post(`${BASE}/tasks/${encodeURIComponent(taskId)}/resume`, {
      checkpoint_id: checkpointId,
    }),

  abortTask: (taskId: string, reason?: string) =>
    client.post(`${BASE}/tasks/${encodeURIComponent(taskId)}/abort`, { confirm: true, reason }),

  streamUrl: (tenantId: string, lastSeq: number) => {
    const params = new URLSearchParams();
    params.set('tenant', tenantId || 'default');
    params.set('last_seq', String(lastSeq));
    return `${BASE}/monitor/stream?${params.toString()}`;
  },
};

export default harnessLoopApi;