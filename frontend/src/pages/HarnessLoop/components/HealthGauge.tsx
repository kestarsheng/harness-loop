import { useMemo } from 'react';
import type { EChartsOption } from 'echarts';
import EChart from './EChart';
import { chartToken } from './chartTheme';

export interface HealthGaugeProps {
  score: number;
  label?: string;
  sampleInsufficient?: boolean;
  height?: number;
}

export default function HealthGauge({
  score,
  label = '健康度',
  sampleInsufficient = false,
  height = 180,
}: HealthGaugeProps) {
  const option = useMemo<EChartsOption>(() => {
    const danger = chartToken('danger');
    const warn = chartToken('warn');
    const primary = chartToken('primary');
    const muted = chartToken('muted');
    const text = chartToken('text');

    return {
      animation: false,
      series: [
        {
          type: 'gauge',
          startAngle: 210,
          endAngle: -30,
          min: 0,
          max: 100,
          radius: '92%',
          center: ['50%', '58%'],
          splitNumber: 4,
          axisLine: {
            lineStyle: {
              width: 10,
              color: [
                [0.4, danger],
                [0.7, warn],
                [1, primary],
              ],
            },
          },
          pointer: {
            width: 3,
            length: '55%',
            itemStyle: { color: text },
          },
          axisTick: {
            distance: -10,
            length: 4,
            lineStyle: { color: muted, width: 1 },
          },
          splitLine: {
            distance: -12,
            length: 8,
            lineStyle: { color: muted, width: 1 },
          },
          axisLabel: { show: false },
          anchor: { show: false },
          title: {
            offsetCenter: [0, '78%'],
            color: muted,
            fontSize: 11,
          },
          detail: {
            offsetCenter: [0, '38%'],
            valueAnimation: false,
            fontSize: 28,
            fontWeight: 600,
            color: text,
            formatter: '{value}',
          },
          data: [{ value: score, name: sampleInsufficient ? `${label}（样本不足）` : label }],
        },
      ],
    };
  }, [score, label, sampleInsufficient]);

  return <EChart option={option} height={height} notMerge />;
}