import { useMemo } from 'react';
import * as echarts from 'echarts';
import type { CustomSeriesRenderItemReturn, EChartsOption } from 'echarts';
import EChart from './EChart';
import type { SubtaskSnapshot, SubtaskStatus } from '../subtask';
import { SUBTASK_STATUS_LABELS } from '../subtask';
import { cssVar } from './chartTheme';

const MAX_ROWS = 12;

const STATUS_INDEX: Record<SubtaskStatus, number> = {
  queued: 0,
  running: 1,
  accepted: 2,
  failed: 3,
  cancelled: 4,
};


export interface DagTimelineProps {
  snapshot: SubtaskSnapshot;
  nowMs: number;
  height?: number;
}

export default function DagTimeline({
  snapshot,
  nowMs,
  height = 200,
}: DagTimelineProps) {
  const option = useMemo<EChartsOption>(() => {
    const border = cssVar('--hlp-border', '#2a2f38');
    const labelColor = cssVar('--hlp-text-secondary', '#9aa3ae');
    const accent = cssVar('--hlp-accent', '#4c8dff');
    const primary = cssVar('--hlp-primary', '#2bd4a8');
    const danger = cssVar('--hlp-danger', '#e5534b');
    const muted = cssVar('--hlp-muted', '#6b7280');

    const rows = snapshot.order
      .map((subtaskId) => snapshot.nodes[subtaskId])
      .filter((node) => node && node.startedAtMs !== null)
      .slice(0, MAX_ROWS);

    if (rows.length === 0) {
      return { animation: false, series: [] };
    }

    const setIds: string[] = [];
    for (const node of rows) {
      const setId = node.parallelSetId ?? 'default';
      if (!setIds.includes(setId)) {
        setIds.push(setId);
      }
    }

    const data = rows.map((node, rowIndex) => {
      const startMs = node.startedAtMs ?? nowMs;
      const endMs = node.endedAtMs ?? (node.status === 'running' ? nowMs : startMs);
      const setIndex = Math.max(0, setIds.indexOf(node.parallelSetId ?? 'default'));
      return [startMs, rowIndex, Math.max(startMs + 1000, endMs), setIndex, STATUS_INDEX[node.status]];
    });

    const minMs = Math.min(...rows.map((node) => node.startedAtMs ?? nowMs));
    const maxMs = Math.max(
      nowMs,
      ...rows.map((node) => node.endedAtMs ?? node.startedAtMs ?? nowMs),
    );

    return {
      animation: false,
      grid: { left: 118, right: 24, top: 12, bottom: 28 },
      tooltip: {
        formatter: (params: unknown) => {
          const item = params as { data: number[] };
          const [startMs, rowIndex, endMs, , statusIndex] = item.data;
          const node = rows[rowIndex];
          if (!node) {
            return '';
          }
          const status = (Object.keys(STATUS_INDEX) as SubtaskStatus[]).find(
            (key) => STATUS_INDEX[key] === statusIndex,
          );
          return [
            `${node.role} · ${node.title || node.subtaskId}`,
            `状态 ${status ? SUBTASK_STATUS_LABELS[status] : node.status}`,
            `attempt ${node.attempt}`,
            `开始 ${new Date(startMs).toLocaleTimeString()}`,
            `结束 ${new Date(endMs).toLocaleTimeString()}`,
            `并行集 ${node.parallelSetId ?? '—'}`,
          ].join('<br/>');
        },
      },
      xAxis: {
        type: 'time',
        min: minMs - 60 * 1000,
        max: maxMs + 60 * 1000,
        axisLine: { lineStyle: { color: border } },
        axisTick: { show: false },
        axisLabel: { color: labelColor, fontSize: 10, hideOverlap: true },
        splitLine: { lineStyle: { color: border, type: 'dashed' } },
      },
      yAxis: {
        type: 'category',
        inverse: true,
        data: rows.map((node) => `${node.role} · ${node.subtaskId}`),
        axisLine: { lineStyle: { color: border } },
        axisTick: { show: false },
        axisLabel: {
          color: labelColor,
          fontSize: 10,
          width: 104,
          overflow: 'truncate',
          formatter: (value: string) =>
            value.length > 16 ? `${value.slice(0, 16)}…` : value,
        },
      },
      series: [
        {
          type: 'custom',
          clip: true,
          data,
          renderItem: (params, api) => {
            const startMs = api.value(0) as number;
            const rowIndex = api.value(1) as number;
            const endMs = api.value(2) as number;
            const setIndex = api.value(3) as number;
            const statusIndex = api.value(4) as number;
            const start = api.coord([startMs, rowIndex]);
            const end = api.coord([endMs, rowIndex]);
            const size = api.size([1, 1]) as number[];
            const height = Math.max(8, size[1] * 0.42);
            const width = Math.max(4, end[0] - start[0]);
            const rect = (echarts.graphic as unknown as {
              clipRectByRect: (
                target: unknown,
                coordSys: unknown,
              ) => { x: number; y: number; width: number; height: number } | undefined;
            }).clipRectByRect(
              {
                x: start[0],
                y: start[1] - height / 2,
                width,
                height,
              },
              params.coordSys,
            );
            if (!rect) {
              return undefined;
            }

            const status = (Object.keys(STATUS_INDEX) as SubtaskStatus[]).find(
              (key) => STATUS_INDEX[key] === statusIndex,
            );
            const fill =
              status === 'failed'
                ? danger
                : status === 'accepted'
                  ? primary
                  : status === 'cancelled'
                    ? muted
                    : accent;

            return {
              type: 'group',
              children: [
                {
                  type: 'rect',
                  shape: rect,
                  style: {
                    fill,
                    opacity:
                      status === 'failed' ? 0.95 : 0.35 + Math.min(3, setIndex) * 0.18,
                  },
                },
              ],
            } as unknown as CustomSeriesRenderItemReturn;
          },
        },
      ],
    };
  }, [snapshot, nowMs]);

  if (snapshot.order.length === 0) {
    return (
      <div style={{ fontSize: 12, color: 'var(--hlp-text-secondary, #9aa3ae)', padding: '24px 0' }}>
        暂无子任务时间线数据
      </div>
    );
  }

  return <EChart option={option} height={height} notMerge />;
}