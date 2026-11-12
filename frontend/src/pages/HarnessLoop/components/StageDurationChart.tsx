import { useMemo } from 'react';
import type { EChartsOption } from 'echarts';
import EChart from './EChart';
import type { StageCode, TaskRuntimeState } from '../types';
import { STAGES, STAGE_LABELS } from '../types';
import { cssVar, stageColors } from './chartTheme';

export interface StageDurationChartProps {
  states: TaskRuntimeState[];
  height?: number;
}

export default function StageDurationChart({ states, height = 220 }: StageDurationChartProps) {
  const option = useMemo<EChartsOption>(() => {
    const colors = stageColors();
    const border = cssVar('--hlp-border', '#2a2f38');
    const labelColor = cssVar('--hlp-text-secondary', '#9aa3ae');

    const totals = new Map<StageCode, { sum: number; count: number }>();
    for (const stage of STAGES) {
      totals.set(stage, { sum: 0, count: 0 });
    }
    for (const state of states) {
      for (const stage of STAGES) {
        const duration = state.stages[stage]?.durationMs ?? 0;
        if (duration > 0) {
          const bucket = totals.get(stage)!;
          bucket.sum += duration;
          bucket.count += 1;
        }
      }
    }

    const averages = STAGES.map((stage) => {
      const bucket = totals.get(stage)!;
      return bucket.count > 0 ? bucket.sum / bucket.count / 1000 : 0;
    });

    return {
      animation: false,
      grid: { left: 96, right: 40, top: 8, bottom: 24 },
      tooltip: {
        trigger: 'axis',
        axisPointer: { type: 'shadow' },
        valueFormatter: (value) => `${Number(value).toFixed(1)}s`,
      },
      xAxis: {
        type: 'value',
        name: '秒',
        nameTextStyle: { color: labelColor, fontSize: 10 },
        axisLine: { show: false },
        axisTick: { show: false },
        splitLine: { lineStyle: { color: border, type: 'dashed' } },
        axisLabel: { color: labelColor, fontSize: 10 },
      },
      yAxis: {
        type: 'category',
        inverse: true,
        data: STAGES.map((stage) => `${stage.toUpperCase()} ${STAGE_LABELS[stage]}`),
        axisLine: { lineStyle: { color: border } },
        axisTick: { show: false },
        axisLabel: { color: labelColor, fontSize: 11 },
      },
      series: [
        {
          type: 'bar',
          barWidth: 12,
          data: STAGES.map((stage, index) => ({
            value: Number(averages[index].toFixed(2)),
            itemStyle: { color: colors[stage], borderRadius: 3 },
          })),
          label: {
            show: true,
            position: 'right',
            color: labelColor,
            fontSize: 10,
            formatter: ({ value }) => (Number(value) > 0 ? `${value}s` : '—'),
          },
        },
      ],
    };
  }, [states]);

  return <EChart option={option} height={height} notMerge />;
}