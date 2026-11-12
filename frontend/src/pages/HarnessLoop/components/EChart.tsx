import { useEffect, useRef } from 'react';
import * as echarts from 'echarts';

export interface EChartProps {
  option: echarts.EChartsOption;
  height?: number | string;
  notMerge?: boolean;
}

export default function EChart({ option, height = 220, notMerge = false }: EChartProps) {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const chartRef = useRef<echarts.ECharts | null>(null);

  useEffect(() => {
    const node = containerRef.current;
    if (!node) {
      return undefined;
    }
    const chart = echarts.init(node);
    chartRef.current = chart;

    const resize = () => chart.resize();
    let observer: ResizeObserver | null = null;
    if (typeof ResizeObserver !== 'undefined') {
      observer = new ResizeObserver(resize);
      observer.observe(node);
    } else {
      window.addEventListener('resize', resize);
    }

    return () => {
      observer?.disconnect();
      window.removeEventListener('resize', resize);
      chart.dispose();
      chartRef.current = null;
    };
  }, []);

  useEffect(() => {
    chartRef.current?.setOption(option, notMerge);
  }, [option, notMerge]);

  return (
    <div
      ref={containerRef}
      style={{ width: '100%', height: typeof height === 'number' ? `${height}px` : height }}
    />
  );
}