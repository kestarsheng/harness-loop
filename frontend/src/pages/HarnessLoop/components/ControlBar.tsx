import { useMemo, useState } from 'react';
import { Alert, Button, Input, Modal, Space, Tag } from 'antd';
import { PauseCircleOutlined, PlayCircleOutlined, StopOutlined } from '@ant-design/icons';
import type { TaskRuntimeState } from '../types';
import { isTerminalStatus, statusLabel } from '../types';

export type ControlAction = 'pause' | 'resume' | 'abort';

export interface ControlBarProps {
  state?: TaskRuntimeState;
  operating?: boolean;
  onAction: (action: ControlAction) => Promise<void>;
}

const ACTION_TITLES: Record<'pause' | 'abort', string> = {
  pause: '暂停任务',
  abort: '中止任务',
};

export default function ControlBar({ state, operating = false, onAction }: ControlBarProps) {
  const [pendingAction, setPendingAction] = useState<'pause' | 'abort' | null>(null);
  const [confirmText, setConfirmText] = useState('');
  const [feedback, setFeedback] = useState<string | null>(null);

  const confirmSuffix = useMemo(() => (state?.taskId ?? '').slice(-4), [state?.taskId]);
  const canConfirm = confirmText.trim() === confirmSuffix && confirmSuffix !== '';
  const terminal = state ? isTerminalStatus(state.status) : true;

  const handleConfirm = async () => {
    if (!pendingAction || !canConfirm) {
      return;
    }
    const action = pendingAction;
    setPendingAction(null);
    setConfirmText('');
    try {
      await onAction(action);
      setFeedback(action === 'pause' ? '已提交暂停，回执事件以 task.paused 为准' : '已提交中止，回执事件以 task.aborted 为准');
    } catch (error) {
      setFeedback(error instanceof Error ? error.message : '操作失败');
    }
  };

  return (
    <div
      style={{
        background: 'var(--hlp-panel, #161a20)',
        border: '1px solid var(--hlp-border, #2a2f38)',
        borderRadius: 'var(--hlp-radius, 10px)',
        padding: 12,
      }}
    >
      <div
        style={{
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          marginBottom: 10,
        }}
      >
        <span style={{ fontSize: 12, color: 'var(--hlp-text-secondary, #9aa3ae)' }}>
          执行控制
        </span>
        <Tag color="default" style={{ marginInlineEnd: 0, fontSize: 10 }}>
          只读视图 · 运维角色可操作
        </Tag>
      </div>

      {state ? (
        <div style={{ fontSize: 12, color: 'var(--hlp-text, #e6e9ef)', marginBottom: 10 }}>
          <span style={{ fontFamily: 'ui-monospace, monospace' }}>{state.taskId}</span>
          <span style={{ marginInlineStart: 8, color: 'var(--hlp-text-secondary, #9aa3ae)' }}>
            {statusLabel(state.status)}
          </span>
        </div>
      ) : (
        <div style={{ fontSize: 12, color: 'var(--hlp-muted, #6b7280)', marginBottom: 10 }}>
          选择任务后可执行暂停 / 恢复 / 中止
        </div>
      )}

      <Space size={8} wrap>
        <Button
          size="small"
          icon={<PauseCircleOutlined />}
          disabled={!state || terminal || state.status === 'paused' || operating}
          onClick={() => {
            setFeedback(null);
            setConfirmText('');
            setPendingAction('pause');
          }}
        >
          暂停
        </Button>
        <Button
          size="small"
          icon={<PlayCircleOutlined />}
          disabled={!state || state.status !== 'paused' || operating}
          loading={operating}
          onClick={async () => {
            setFeedback(null);
            try {
              await onAction('resume');
              setFeedback('已提交恢复，回执事件以 task.resumed 为准');
            } catch (error) {
              setFeedback(error instanceof Error ? error.message : '操作失败');
            }
          }}
        >
          恢复
        </Button>
        <Button
          size="small"
          danger
          icon={<StopOutlined />}
          disabled={!state || terminal || operating}
          onClick={() => {
            setFeedback(null);
            setConfirmText('');
            setPendingAction('abort');
          }}
        >
          中止
        </Button>
      </Space>

      {feedback && (
        <Alert
          style={{ marginTop: 10 }}
          type={feedback.includes('失败') ? 'error' : 'info'}
          message={feedback}
          showIcon
        />
      )}

      <Modal
        title={pendingAction ? ACTION_TITLES[pendingAction] : ''}
        open={pendingAction !== null}
        okText="确认"
        cancelText="取消"
        okButtonProps={{ danger: pendingAction === 'abort', disabled: !canConfirm, loading: operating }}
        onOk={handleConfirm}
        onCancel={() => {
          setPendingAction(null);
          setConfirmText('');
        }}
        destroyOnClose
      >
        <div style={{ fontSize: 13, marginBottom: 8 }}>
          该操作会写入审计，并以事件回执确认结果。请输入任务 ID 后 4 位确认：
          <span style={{ fontFamily: 'ui-monospace, monospace', marginInlineStart: 6, color: 'var(--hlp-primary, #2bd4a8)' }}>
            {confirmSuffix || '—'}
          </span>
        </div>
        <Input
          value={confirmText}
          placeholder="任务 ID 后 4 位"
          maxLength={8}
          onChange={(event) => setConfirmText(event.target.value)}
          onPressEnter={handleConfirm}
        />
      </Modal>
    </div>
  );
}