import { useMemo } from 'react';
import type { EChartsOption } from 'echarts';
import EChart from './EChart';
import type { ModelFallbackTrigger, ModelUsageState } from '../modelUsage';
import { computeFallbackTrend, computeModelDistribution } from '../modelUsage';
import { cssVar } from './chartTheme';

export interface ModelDistributionChartProps {
  state: ModelUsageState;
  nowMs: number;
}

const TRIGGER_LABELS: Record<ModelFallbackTrigger, string> = {
  timeout: '超时',
  rate_limit: '限流',
  error: '错误',
  budget: '预算',
  manual: '手动',
  unknown: '未分类',
};

function clockLabel(timestampMs: number): string {
  return new Date(timestampMs).toLocaleTimeString('zh-CN', {
    hour12: false,
    hour: '2-digit',
    minute: '2-digit',
  });
}

export default function ModelDistributionChart({
  state,
  nowMs,
}: ModelDistributionChartProps) {
  const entries = useMemo(() => computeModelDistribution(state, 5), [state]);
  const trend = useMemo(() => computeFallbackTrend(state, nowMs), [state, nowMs]);

  const distributionOption = useMemo<EChartsOption>(() => {
    const accent = cssVar('--hlp-accent', '#4c8dff');
    const border = cssVar('--hlp-border', '#2a2f38');
    const labelColor = cssVar('--hlp-text-secondary', '#9aa3ae');
    const text = cssVar('--hlp-text', '#e6e9ef');

    return {
      animation: false,
      grid: { left: 32, right: 16, top: 22, bottom: 28 },
      tooltip: { trigger: 'axis', axisPointer: { type: 'shadow' } },
      xAxis: {
        type: 'category',
        data: entries.map((entry) => entry.model),
        axisLine: { lineStyle: { color: border } },
        axisTick: { show: false },
        axisLabel: {
          color: labelColor,
          fontSize: 10,
          interval: 0,
          rotate: entries.length > 3 ? 18 : 0,
        },
      },
      yAxis: {
        type: 'value',
        minInterval: 1,
        axisLine: { show: false },
        axisTick: { show: false },
        splitLine: { lineStyle: { color: border, type: 'dashed' } },
        axisLabel: { color: labelColor, fontSize: 10 },
      },
      series: [
        {
          type: 'bar',
          name: '选择次数',
          data: entries.map((entry) => entry.calls),
          barMaxWidth: 26,
          itemStyle: { color: accent, opacity: 0.85 },
          label: { show: true, position: 'top', color: text, fontSize: 10 },
        },
      ],
    };
  }, [entries]);

  const trendOption = useMemo<EChartsOption>(() => {
    const border = cssVar('--hlp-border', '#2a2f38');
    const labelColor = cssVar('--hlp-text-secondary', '#9aa3ae');
    const primary = cssVar('--hlp-primary', '#2bd4a8');
    const accent = cssVar('--hlp-accent', '#4c8dff');
    const warn = cssVar('--hlp-warn', '#e8b339');
    const danger = cssVar('--hlp-danger', '#e5534b');
    const muted = cssVar('--hlp-muted', '#6b7280');

    const colorOf = (trigger: ModelFallbackTrigger): string => {
      switch (trigger) {
        case 'timeout':
          return warn;
        case 'rate_limit':
          return accent;
        case 'error':
          return danger;
        case 'budget':
          return primary;
        case 'manual':
          return primary;
        default:
          return muted;
      }
    };

    return {
      animation: false,
      grid: { left: 32, right: 16, top: 26, bottom: 24 },
      legend: {
        top: 0,
        right: 0,
        itemWidth: 8,
        itemHeight: 8,
        textStyle: { color: labelColor, fontSize: 10 },
        data: trend.triggers.map((trigger) => TRIGGER_LABELS[trigger]),
      },
      tooltip: { trigger: 'axis' },
      xAxis: {
        type: 'category',
        data: trend.bucketStartMs.map(clockLabel),
        axisLine: { lineStyle: { color: border } },
        axisTick: { show: false },
        axisLabel: { color: labelColor, fontSize: 10, interval: 'auto' },
      },
      yAxis: {
        type: 'value',
        minInterval: 1,
        axisLine: { show: false },
        axisTick: { show: false },
        splitLine: { lineStyle: { color: border, type: 'dashed' } },
        axisLabel: { color: labelColor, fontSize: 10 },
      },
      series: trend.series.map((entry) => ({
        type: 'line',
        name: TRIGGER_LABELS[entry.trigger],
        data: entry.values,
        smooth: true,
        symbol: 'circle',
        symbolSize: 4,
        lineStyle: { width: 1.5, color: colorOf(entry.trigger) },
        itemStyle: { color: colorOf(entry.trigger) },
      })),
    };
  }, [trend]);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
      <div style={{ fontSize: 11, color: 'var(--hlp-text-secondary, #9aa3ae)' }}>
        模型分布（按选择次数 Top5）
      </div>
      {entries.length > 0 ? (
        <EChart option={distributionOption} height={132} notMerge />
      ) : (
        <div style={{ fontSize: 12, color: 'var(--hlp-muted, #6b7280)', padding: '28px 0' }}>
          暂无模型选择记录
        </div>
      )}

      <div style={{ fontSize: 11, color: 'var(--hlp-text-secondary, #9aa3ae)', marginTop: 4 }}>
        回退趋势（最近 1 小时，按触发原因分组）
      </div>
      {trend.triggers.length > 0 ? (
        <EChart option={trendOption} height={140} notMerge />
      ) : (
        <div style={{ fontSize: 12, color: 'var(--hlp-muted, #6b7280)', padding: '20px 0' }}>
          最近 1 小时无模型回退
        </div>
      )}
    </div>
  );
}