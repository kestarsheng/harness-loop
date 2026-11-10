import { useState, useEffect, useCallback } from 'react';
import {
  Table, Tag, Button, Space, Form, Input, Select, InputNumber,
  Statistic, Row, Col, Card, Alert, Steps, Progress, Empty, Tooltip, message
} from 'antd';
import {
  RocketOutlined, ThunderboltOutlined, CheckCircleOutlined,
  ReloadOutlined, TrophyOutlined
} from '@ant-design/icons';
import { harnessApi } from '../../api/harness';
import { promptApi } from '../../api/prompt';

// ==================== Types ====================

interface Dataset {
  id: string;
  name: string;
  case_count: number;
}

interface PromptEntry {
  id: string;
  key: string;
  name: string;
}

interface OptimizeRound {
  round: number;
  prompt_text: string;
  score: number;
  improved: boolean;
  suggestion: string;
  version_id: string;
}

interface OptimizeResult {
  best_version_id: string;
  best_score: number;
  original_score: number;
  rounds: number;
  strategy: string;
  history: OptimizeRound[];
}

// ==================== Component ====================

export default function AutoOptimizerPanel() {
  // Config state
  const [prompts, setPrompts] = useState<PromptEntry[]>([]);
  const [datasets, setDatasets] = useState<Dataset[]>([]);
  const [configLoading, setConfigLoading] = useState(false);

  // Form state
  const [form] = Form.useForm();
  const [running, setRunning] = useState(false);
  const [result, setResult] = useState<OptimizeResult | null>(null);

  // Adopt state
  const [adopting, setAdopting] = useState(false);

  // ==================== Load Config ====================

  const loadConfig = useCallback(async () => {
    setConfigLoading(true);
    try {
      const [promptRes, datasetRes] = await Promise.all([
        promptApi.listPrompts() as any,
        harnessApi.listDatasets() as any,
      ]);
      setPrompts(promptRes?.prompts || []);
      setDatasets(datasetRes?.datasets || []);
    } catch {
      setPrompts([]);
      setDatasets([]);
    } finally {
      setConfigLoading(false);
    }
  }, []);

  useEffect(() => {
    loadConfig();
  }, [loadConfig]);

  // ==================== Run Optimization ====================

  const handleRun = async (values: {
    prompt_key: string;
    dataset_id: string;
    metric?: string;
    strategy?: string;
    max_rounds?: number;
    no_improve_limit?: number;
    score_threshold?: number;
    model?: string;
    candidates_per_round?: number;
    population_size?: number;
  }) => {
    setRunning(true);
    setResult(null);
    try {
      const res = await harnessApi.runPromptOptimization(values) as any;
      setResult(res || null);
      message.success(`优化完成！${res?.rounds || 0} 轮，最佳得分 ${(res?.best_score * 100 || 0).toFixed(1)}%`);
    } catch {
      message.error('优化运行失败');
      setResult(null);
    } finally {
      setRunning(false);
    }
  };

  // ==================== Adopt Version ====================

  const handleAdopt = async (versionId: string) => {
    setAdopting(true);
    try {
      await harnessApi.adoptPromptVersion(versionId);
      message.success('已采纳为活跃版本！');
    } catch {
      message.error('采纳失败');
    } finally {
      setAdopting(false);
    }
  };

  // ==================== Score Color ====================

  const scoreColor = (score: number) => {
    if (score >= 0.8) return '#52c41a';
    if (score >= 0.5) return '#faad14';
    return '#ff4d4f';
  };

  // ==================== Improvement % ====================

  const improvementPct = result && result.original_score > 0
    ? ((result.best_score - result.original_score) / result.original_score * 100)
    : 0;

  // ==================== Round Columns ====================

  const roundColumns = [
    {
      title: '轮次', dataIndex: 'round', key: 'round', width: 60,
      render: (r: number) => <strong>#{r}</strong>,
    },
    {
      title: '得分', dataIndex: 'score', key: 'score', width: 100,
      render: (s: number) => <span style={{ color: scoreColor(s), fontWeight: 600 }}>{(s * 100).toFixed(1)}%</span>,
    },
    {
      title: '改进', dataIndex: 'improved', key: 'improved', width: 80,
      render: (improved: boolean) => improved
        ? <Tag color="success" icon={<CheckCircleOutlined />}>↑</Tag>
        : <Tag color="default">—</Tag>,
    },
    {
      title: '建议', dataIndex: 'suggestion', key: 'suggestion', ellipsis: true,
      render: (v: string) => <Tooltip title={v}><span>{v || '-'}</span></Tooltip>,
    },
    {
      title: 'Prompt', dataIndex: 'prompt_text', key: 'prompt_text', ellipsis: true,
      render: (v: string) => <Tooltip title={v}><code>{v ? (v.length > 40 ? v.slice(0, 40) + '...' : v) : '-'}</code></Tooltip>,
    },
    {
      title: '版本ID', dataIndex: 'version_id', key: 'version_id', width: 100,
      render: (id: string) => id ? id.slice(0, 8) : '-',
    },
  ];

  // ==================== Render ====================

  return (
    <div>
      <Alert
        message="自动优化器基于评估数据集，使用 TextGrad（文本梯度）或 GEPA（反射进化）策略自动迭代改进 Prompt"
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
      />

      <Row gutter={24}>
        {/* Config Form */}
        <Col span={10}>
          <Card title="优化配置" size="small">
            <Form
              form={form}
              layout="vertical"
              onFinish={handleRun}
              initialValues={{
                strategy: 'textgrad',
                metric: 'accuracy',
                max_rounds: 5,
                no_improve_limit: 3,
                score_threshold: 0.9,
                candidates_per_round: 3,
                population_size: 5,
              }}
            >
              <Form.Item name="prompt_key" label="Prompt" rules={[{ required: true, message: '请选择 Prompt' }]}>
                <Select
                  placeholder="选择要优化的 Prompt"
                  loading={configLoading}
                  showSearch
                  optionFilterProp="label"
                  options={prompts.map(p => ({
                    value: p.key, label: `${p.name} (${p.key})`,
                  }))}
                />
              </Form.Item>

              <Form.Item name="dataset_id" label="评估数据集" rules={[{ required: true, message: '请选择数据集' }]}>
                <Select
                  placeholder="选择评估数据集"
                  loading={configLoading}
                  options={datasets.map(d => ({
                    value: d.id, label: `${d.name} (${d.case_count}条)`,
                  }))}
                />
              </Form.Item>

              <Form.Item name="strategy" label="优化策略">
                <Select options={[
                  { value: 'textgrad', label: 'TextGrad — 文本梯度优化' },
                  { value: 'gepa', label: 'GEPA — 反射进化 + 种群' },
                ]} />
              </Form.Item>

              <Form.Item name="metric" label="评估指标">
                <Select options={[
                  { value: 'accuracy', label: '准确率 (accuracy)' },
                  { value: 'similarity', label: '相似度 (similarity)' },
                  { value: 'latency', label: '延迟 (latency)' },
                ]} />
              </Form.Item>

              <Form.Item name="model" label="模型（可选）">
                <Input placeholder="例如：gpt-4o-mini" />
              </Form.Item>

              <Row gutter={12}>
                <Col span={12}>
                  <Form.Item name="max_rounds" label="最大轮次">
                    <InputNumber min={1} max={50} style={{ width: '100%' }} />
                  </Form.Item>
                </Col>
                <Col span={12}>
                  <Form.Item name="no_improve_limit" label="无改进停止">
                    <InputNumber min={1} max={20} style={{ width: '100%' }} />
                  </Form.Item>
                </Col>
              </Row>

              <Row gutter={12}>
                <Col span={12}>
                  <Form.Item name="score_threshold" label="目标分数">
                    <InputNumber min={0} max={1} step={0.05} style={{ width: '100%' }} />
                  </Form.Item>
                </Col>
                <Col span={12}>
                  <Form.Item name="candidates_per_round" label="每轮候选数">
                    <InputNumber min={1} max={10} style={{ width: '100%' }} />
                  </Form.Item>
                </Col>
              </Row>

              <Form.Item noStyle shouldUpdate={(prev, cur) => prev.strategy !== cur.strategy}>
                {({ getFieldValue }) =>
                  getFieldValue('strategy') === 'gepa' ? (
                    <Form.Item name="population_size" label="种群大小 (GEPA)">
                      <InputNumber min={2} max={20} style={{ width: '100%' }} />
                    </Form.Item>
                  ) : null
                }
              </Form.Item>

              <Form.Item>
                <Space>
                  <Button
                    type="primary"
                    htmlType="submit"
                    icon={<RocketOutlined />}
                    loading={running}
                    size="large"
                  >
                    {running ? '优化中...' : '开始优化'}
                  </Button>
                  <Button icon={<ReloadOutlined />} onClick={loadConfig}>刷新配置</Button>
                </Space>
              </Form.Item>
            </Form>
          </Card>
        </Col>

        {/* Results */}
        <Col span={14}>
          {running && (
            <Card style={{ textAlign: 'center', padding: 40 }}>
              <RocketOutlined style={{ fontSize: 48, color: '#1677ff', marginBottom: 16 }} />
              <div style={{ fontSize: 18, fontWeight: 600, marginBottom: 8 }}>优化运行中...</div>
              <div style={{ color: '#999' }}>正在迭代改进 Prompt，请稍候</div>
            </Card>
          )}

          {!running && result && (
            <div>
              {/* Summary */}
              <Card size="small" style={{ marginBottom: 16 }}>
                <Row gutter={16} align="middle">
                  <Col span={5}>
                    <Statistic
                      title="原始得分"
                      value={result.original_score * 100}
                      precision={1}
                      suffix="%"
                      valueStyle={{ color: scoreColor(result.original_score) }}
                    />
                  </Col>
                  <Col span={5}>
                    <Statistic
                      title="最佳得分"
                      value={result.best_score * 100}
                      precision={1}
                      suffix="%"
                      valueStyle={{ color: scoreColor(result.best_score) }}
                      prefix={<TrophyOutlined />}
                    />
                  </Col>
                  <Col span={5}>
                    <Statistic
                      title="提升"
                      value={improvementPct}
                      precision={1}
                      suffix="%"
                      valueStyle={{ color: improvementPct > 0 ? '#52c41a' : '#999' }}
                      prefix={improvementPct > 0 ? '+' : ''}
                    />
                  </Col>
                  <Col span={4}>
                    <Statistic title="轮次" value={result.rounds} />
                  </Col>
                  <Col span={5}>
                    <div style={{ textAlign: 'center' }}>
                      <div style={{ color: 'rgba(0,0,0,0.45)', fontSize: 14, marginBottom: 4 }}>策略</div>
                      <Tag color={result.strategy === 'gepa' ? 'purple' : 'blue'} style={{ fontSize: 14 }}>
                        {result.strategy === 'gepa' ? 'GEPA' : 'TextGrad'}
                      </Tag>
                    </div>
                  </Col>
                </Row>
              </Card>

              {/* Score Progress */}
              <Card size="small" title="得分趋势" style={{ marginBottom: 16 }}>
                <Steps
                  size="small"
                  items={result.history.map((r, i) => ({
                    title: `R${r.round}`,
                    description: `${(r.score * 100).toFixed(1)}%`,
                    status: r.improved ? 'finish' : 'wait',
                    icon: r.improved ? <CheckCircleOutlined style={{ color: '#52c41a' }} /> : undefined,
                  }))}
                />
                <div style={{ marginTop: 12 }}>
                  <Progress
                    percent={Math.round(result.best_score * 100)}
                    strokeColor={scoreColor(result.best_score)}
                    format={() => `最佳 ${(result.best_score * 100).toFixed(1)}%`}
                  />
                </div>
              </Card>

              {/* Adopt Button */}
              {result.best_version_id && (
                <Alert
                  message="发现更优版本"
                  description={
                    <Space>
                      <span>最佳版本得分 {(result.best_score * 100).toFixed(1)}%，是否采纳为活跃版本？</span>
                      <Button
                        type="primary"
                        icon={<ThunderboltOutlined />}
                        loading={adopting}
                        onClick={() => handleAdopt(result.best_version_id)}
                      >
                        采纳此版本
                      </Button>
                    </Space>
                  }
                  type="success"
                  showIcon
                  style={{ marginBottom: 16 }}
                />
              )}

              {/* Round History Table */}
              <Card size="small" title="迭代历史">
                <Table
                  columns={roundColumns}
                  dataSource={result.history}
                  rowKey="round"
                  size="small"
                  pagination={false}
                  locale={{ emptyText: <Empty description="无迭代记录" /> }}
                />
              </Card>
            </div>
          )}

          {!running && !result && (
            <Card style={{ textAlign: 'center', padding: 60 }}>
              <RocketOutlined style={{ fontSize: 48, color: '#d9d9d9', marginBottom: 16 }} />
              <div style={{ fontSize: 16, color: '#999' }}>选择 Prompt 和数据集，开始自动优化</div>
              <div style={{ fontSize: 13, color: '#bbb', marginTop: 8 }}>
                TextGrad：基于 LLM 反馈的文本梯度优化<br />
                GEPA：反射进化 + 种群搜索
              </div>
            </Card>
          )}
        </Col>
      </Row>
    </div>
  );
}
