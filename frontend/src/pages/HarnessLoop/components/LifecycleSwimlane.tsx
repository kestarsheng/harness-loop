import { useMemo } from 'react';
import * as echarts from 'echarts';
import type { CustomSeriesRenderItemReturn, EChartsOption } from 'echarts';
import EChart from './EChart';
import type { StageCode, TaskRuntimeState } from '../types';
import { STAGES } from '../types';
import { cssVar, stageColor } from './chartTheme';

const STAGE_ORDER: Record<StageCode, number> = { a: 0, b: 1, c: 2, d: 3, e: 4 };
const MAX_ROWS = 14;

export interface LifecycleSwimlaneProps {
  states: TaskRuntimeState[];
  height?: number;
}

function rollbackTargets(state: TaskRuntimeState): Set<StageCode> {
  const targets = new Set<StageCode>();
  for (const transition of state.transitions) {
    const from = STAGE_ORDER[transition.from as StageCode];
    const to = STAGE_ORDER[transition.to as StageCode];
    if (from !== undefined && to !== undefined && to < from) {
      targets.add(transition.to as StageCode);
    }
  }
  return targets;
}

export default function LifecycleSwimlane({ states, height = 240 }: LifecycleSwimlaneProps) {
  const option = useMemo<EChartsOption>(() => {
    const rows = states.slice(0, MAX_ROWS);
    const colors: Record<StageCode, string> = {
      a: stageColor('a'),
      b: stageColor('b'),
      c: stageColor('c'),
      d: stageColor('d'),
      e: stageColor('e'),
    };
    const danger = cssVar('--hlp-danger', '#e5534b');
    const border = cssVar('--hlp-border', '#2a2f38');
    const labelColor = cssVar('--hlp-text-secondary', '#9aa3ae');

    const data: Array<[number, number, number, number]> = [];
    rows.forEach((state, rowIndex) => {
      const rollbackStages = rollbackTargets(state);
      STAGES.forEach((stage, stageIndex) => {
        const stageState = state.stages[stage];
        if (!stageState || stageState.attempts === 0) {
          return;
        }
        data.push([
          stageIndex,
          rowIndex,
          stageState.lastAttempt || stageState.attempts,
          rollbackStages.has(stage) ? 1 : 0,
        ]);
      });
    });

    return {
      animation: false,
      grid: { left: 84, right: 24, top: 16, bottom: 28 },
      tooltip: {
        formatter: (params: unknown) => {
          const item = params as { data: [number, number, number, number] };
          const [stageIndex, rowIndex, attempts, rollback] = item.data;
          return `任务 ${rows[rowIndex]?.taskId ?? ''}<br/>阶段 ${STAGES[stageIndex]?.toUpperCase()}<br/>attempt ${attempts}${
            rollback ? '<br/>含回退' : ''
          }`;
        },
      },
      xAxis: {
        type: 'category',
        data: STAGES.map((stage) => stage.toUpperCase()),
        axisLine: { lineStyle: { color: border } },
        axisTick: { show: false },
        axisLabel: { color: labelColor, fontSize: 11 },
      },
      yAxis: {
        type: 'category',
        inverse: true,
        data: rows.map((state) => state.taskId.slice(0, 8)),
        axisLine: { lineStyle: { color: border } },
        axisTick: { show: false },
        axisLabel: {
          color: labelColor,
          fontSize: 11,
          fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
        },
      },
      series: [
        {
          type: 'custom',
          clip: true,
          data,
          renderItem: (params, api) => {
            const stageIndex = api.value(0) as number;
            const rowIndex = api.value(1) as number;
            const attempts = api.value(2) as number;
            const rollback = api.value(3) as number;
            const point = api.coord([stageIndex, rowIndex]);
            const size = api.size([1, 1]) as number[];
            const width = Math.max(10, size[0] * 0.62);
            const blockHeight = Math.max(8, size[1] * 0.42);
            const rect = (echarts.graphic as unknown as {
              clipRectByRect: (target: unknown, coordSys: unknown) => { x: number; y: number; width: number; height: number } | undefined;
            }).clipRectByRect(
              {
                x: point[0] - width / 2,
                y: point[1] - blockHeight / 2,
                width,
                height: blockHeight,
              },
              params.coordSys,
            );
            if (!rect) {
              return undefined;
            }
            const children: Array<Record<string, unknown>> = [
              {
                type: 'rect',
                shape: rect,
                style: {
                  fill: colors[STAGES[stageIndex]],
                  opacity: 0.35 + Math.min(attempts, 4) * 0.15,
                },
              },
            ];
            if (rollback) {
              children.push({
                type: 'circle',
                shape: { cx: rect.x + rect.width - 4, cy: rect.y + 4, r: 3 },
                style: { fill: danger },
              });
            }
            return { type: 'group', children } as unknown as CustomSeriesRenderItemReturn;
          },
        },
      ],
    };
  }, [states]);

  return <EChart option={option} height={height} notMerge />;
}