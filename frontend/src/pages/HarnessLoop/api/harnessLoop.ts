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