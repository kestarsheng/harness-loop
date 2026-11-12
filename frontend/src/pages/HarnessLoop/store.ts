import { create } from 'zustand';

export type StatusFilter = 'all' | 'running' | 'waiting_acceptance' | 'retrying' | 'failed' | 'completed';

interface MonitorUiState {
  selectedTaskId: string | null;
  anomalyTaskId: string | null;
  anomalyTraceId: string | null;
  statusFilter: StatusFilter;
  selectTask: (taskId: string | null) => void;
  openAnomaly: (taskId: string, traceId?: string) => void;
  closeAnomaly: () => void;
  setStatusFilter: (filter: StatusFilter) => void;
}

export const useMonitorStore = create<MonitorUiState>((set) => ({
  selectedTaskId: null,
  anomalyTaskId: null,
  anomalyTraceId: null,
  statusFilter: 'all',
  selectTask: (taskId) => set({ selectedTaskId: taskId }),
  openAnomaly: (taskId, traceId) =>
    set({ anomalyTaskId: taskId, anomalyTraceId: traceId ?? null }),
  closeAnomaly: () => set({ anomalyTaskId: null, anomalyTraceId: null }),
  setStatusFilter: (filter) => set({ statusFilter: filter }),
}));