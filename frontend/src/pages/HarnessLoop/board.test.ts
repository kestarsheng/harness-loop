import { describe, expect, it } from 'vitest';
import type { HarnessEvent, HarnessTaskView, TaskRuntimeState } from './types';
import {
  createTaskState,
  hydrateTaskState,
  reduceMonitorState,
  reduceTaskState,
  selectBoardTasks,
  taskDisplayTitle,
  TERMINAL_CARD_LIMIT,
} from './types';

function makeState(
  taskId: string,
  overrides: Partial<TaskRuntimeState> = {},
): TaskRuntimeState {
  return createTaskState(taskId, overrides);
}

function makeEvent(
  taskId: string,
  seq: number,
  overrides: Partial<HarnessEvent> = {},
): HarnessEvent {
  return {
    event_id: `evt-${taskId}-${seq}`,
    event_type: 'stage.entered',
    task_id: taskId,
    tenant_id: 'tenant-a',
    seq,
    timestamp_ms: 1_000 + seq * 100,
    stage: 'c',
    payload: '{}',
    ...overrides,
  };
}

describe('taskDisplayTitle', () => {
  it('优先使用 terminal_label', () => {
    const state = makeState('task-1', {
      terminalLabel: 'hlp-dashboard:~/repo',
      goal: '补全登录模块单测',
    });
    expect(taskDisplayTitle(state)).toBe('hlp-dashboard:~/repo');
  });

  it('没有 terminal_label 时回退到 goal 并截断', () => {
    const goal = '让大屏在不按项目过滤的前提下展示同租户全部活跃 agent 并保留终态';
    const state = makeState('task-1', { goal });
    const title = taskDisplayTitle(state, 10);
    expect(title).toBe(`${Array.from(goal).slice(0, 10).join('')}…`);
  });

  it('terminal_label 与 goal 都为空时回退到 task_id', () => {
    const state = makeState('task-9b9a45beca750c67');
    expect(taskDisplayTitle(state)).toBe('task-9b9a45b');
  });
});

describe('selectBoardTasks', () => {
  it('保留全部活跃任务，只保留最近 20 个终态并排在活跃任务之后', () => {
    const active = [
      makeState('running-old', { status: 'running', updatedAtMs: 1_000 }),
      makeState('retrying-new', { status: 'retrying', updatedAtMs: 3_000 }),
      makeState('paused', { status: 'paused', updatedAtMs: 2_000 }),
    ];
    const terminal = Array.from({ length: TERMINAL_CARD_LIMIT + 5 }, (_, index) =>
      makeState(`done-${index}`, {
        status: index % 2 === 0 ? 'completed' : 'failed',
        updatedAtMs: 10_000 + index,
      }),
    );

    const selected = selectBoardTasks([...terminal, ...active]);

    expect(selected.slice(0, 3).map((state) => state.taskId)).toEqual([
      'retrying-new',
      'paused',
      'running-old',
    ]);
    expect(selected).toHaveLength(3 + TERMINAL_CARD_LIMIT);
    expect(selected[3].taskId).toBe('done-24');
    expect(selected[selected.length - 1].taskId).toBe('done-5');
  });

  it('按状态过滤后仍只保留最近 20 个终态', () => {
    const completed = Array.from({ length: 25 }, (_, index) =>
      makeState(`done-${index}`, {
        status: 'completed',
        updatedAtMs: index,
      }),
    );
    const selected = selectBoardTasks(completed);
    expect(selected).toHaveLength(TERMINAL_CARD_LIMIT);
    expect(selected[0].taskId).toBe('done-24');
  });
});

describe('终端身份与归约', () => {
  it('hydrateTaskState 映射 session_id / terminal_label', () => {
    const view: HarnessTaskView = {
      task_id: 'task-1',
      tenant_id: 'tenant-a',
      goal: 'goal',
      status: 'running',
      current_stage: 'b',
      session_id: 'sess-1',
      terminal_label: 'host:~/work',
    };

    const state = hydrateTaskState(view);

    expect(state.sessionId).toBe('sess-1');
    expect(state.terminalLabel).toBe('host:~/work');
    expect(state.goal).toBe('goal');
  });

  it('事件 payload 携带终端身份时也能补齐', () => {
    const event = makeEvent('task-1', 1, {
      payload: JSON.stringify({
        session_id: 'sess-2',
        terminal_label: 'host2:~/repo',
      }),
    });

    const state = reduceTaskState(undefined, event);

    expect(state.sessionId).toBe('sess-2');
    expect(state.terminalLabel).toBe('host2:~/repo');
  });

  it('SSE 批量事件按 task_id 归约，互不串台', () => {
    const events = [
      makeEvent('task-a', 1),
      makeEvent('task-b', 1),
      makeEvent('task-a', 2),
    ];

    const states = reduceMonitorState({}, events);

    expect(Object.keys(states).sort()).toEqual(['task-a', 'task-b']);
    expect(states['task-a'].eventCount).toBe(2);
    expect(states['task-b'].eventCount).toBe(1);
  });
});
