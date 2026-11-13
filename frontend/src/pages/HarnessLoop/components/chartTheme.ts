import type { StageCode } from '../types';
import { STAGES } from '../types';

export const STAGE_COLOR_VARS: Record<StageCode, string> = {
  a: 'var(--hlp-stage-a, #2bd4a8)',
  b: 'var(--hlp-stage-b, #4c8dff)',
  c: 'var(--hlp-stage-c, #8a7dff)',
  d: 'var(--hlp-stage-d, #e8b339)',
  e: 'var(--hlp-stage-e, #6b7280)',
};

const STAGE_FALLBACKS: Record<StageCode, string> = {
  a: '#2bd4a8',
  b: '#4c8dff',
  c: '#8a7dff',
  d: '#e8b339',
  e: '#6b7280',
};

export const CHART_FALLBACKS = {
  bg: '#0f1115',
  panel: '#161a20',
  border: '#2a2f38',
  text: '#e6e9ef',
  textSecondary: '#9aa3ae',
  muted: '#6b7280',
  primary: '#2bd4a8',
  accent: '#4c8dff',
  warn: '#e8b339',
  danger: '#e5534b',
};

export type ChartToken = keyof typeof CHART_FALLBACKS;

export function cssVar(name: string, fallback: string): string {
  if (typeof window === 'undefined' || typeof document === 'undefined') {
    return fallback;
  }
  const value = window
    .getComputedStyle(document.documentElement)
    .getPropertyValue(name)
    .trim();
  return value || fallback;
}

export function chartToken(token: ChartToken): string {
  return cssVar(`--hlp-${token}`, CHART_FALLBACKS[token]);
}

export function stageColor(stage: StageCode): string {
  return cssVar(`--hlp-stage-${stage}`, STAGE_FALLBACKS[stage]);
}

export function stageColors(): Record<StageCode, string> {
  const colors = {} as Record<StageCode, string>;
  for (const stage of STAGES) {
    colors[stage] = stageColor(stage);
  }
  return colors;
}

export const baseChartTextStyle = {
  color: CHART_FALLBACKS.textSecondary,
  fontSize: 11,
};