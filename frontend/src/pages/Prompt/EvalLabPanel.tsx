import { useState, useEffect, useCallback } from 'react';
import {
  Table, Tag, Button, Space, Modal, Form, Input, Select,
  Popconfirm, Badge, Empty, Statistic, Row, Col, Alert, message, Tabs, Tooltip, Card
} from 'antd';
import {
  PlusOutlined, DeleteOutlined, ExperimentOutlined,
  DatabaseOutlined, ImportOutlined, ReloadOutlined, BarChartOutlined
} from '@ant-design/icons';
import { harnessApi } from '../../api/harness';

// ==================== Types ====================

interface Dataset {
  id: string;
  name: string;
  description: string;
  tenant_id: string;
  case_count: number;
  created_at: number;
  updated_at: number;
}

interface DatasetCase {
  id: string;
  dataset_id: string;
  input: string;
  expected: string;
  context: string;
  tags: string[];
  created_at: number;
}

interface Experiment {
  id: string;
  name: string;
  dataset_id: string;
  config_type: string;
  config_ref: string;
  model: string;
  status: string;
  avg_score: number;
  case_count: number;
  created_at: number;
  completed_at: number;
}

interface ExperimentResult {
  id: string;
  experiment_id: string;
  case_id: string;
  output: string;
  score: number;
  passed: boolean;
  latency_ms: number;
  tokens: number;
  scores: string;
  error: string;
}

interface ExperimentSummary {
  id: string;
  name: string;
  config_type: string;
  config_ref: string;
  avg_score: number;
  case_count: number;
  passed_count: number;
  failed_count: number;
  status: string;
}

// ==================== Component ====================

export default function EvalLabPanel() {
  const [activeTab, setActiveTab] = useState('datasets');

  // Dataset state
  const [datasets, setDatasets] = useState<Dataset[]>([]);
  const [datasetsLoading, setDatasetsLoading] = useState(false);
  const [createDatasetOpen, setCreateDatasetOpen] = useState(false);
  const [datasetForm] = Form.useForm();

  // Case state
  const [selectedDataset, setSelectedDataset] = useState<Dataset | null>(null);
  const [cases, setCases] = useState<DatasetCase[]>([]);
  const [casesLoading, setCasesLoading] = useState(false);
  const [addCaseOpen, setAddCaseOpen] = useState(false);
  const [caseForm] = Form.useForm();
  const [importOpen, setImportOpen] = useState(false);
  const [importForm] = Form.useForm();

  // Experiment state
  const [experiments, setExperiments] = useState<Experiment[]>([]);
  const [experimentsLoading, setExperimentsLoading] = useState(false);
  const [runExperimentOpen, setRunExperimentOpen] = useState(false);
  const [experimentForm] = Form.useForm();

  // Results state
  const [resultsExperiment, setResultsExperiment] = useState<Experiment | null>(null);
  const [results, setResults] = useState<ExperimentResult[]>([]);
  const [resultsLoading, setResultsLoading] = useState(false);

  // Compare state
  const [compareIds, setCompareIds] = useState<string[]>([]);
  const [compareResult, setCompareResult] = useState<{ experiments: ExperimentSummary[]; best_experiment_id: string } | null>(null);
  const [compareLoading, setCompareLoading] = useState(false);

  // ==================== Data Loading ====================

  const loadDatasets = useCallback(async () => {
    setDatasetsLoading(true);
    try {
      const res = await harnessApi.listDatasets() as any;
      setDatasets(res?.datasets || []);
    } catch {
      setDatasets([]);
    } finally {
      setDatasetsLoading(false);
    }
  }, []);

  const loadExperiments = useCallback(async () => {
    setExperimentsLoading(true);
    try {
      const res = await harnessApi.listExperiments() as any;
      setExperiments(res?.experiments || []);
    } catch {
      setExperiments([]);
    } finally {
      setExperimentsLoading(false);
    }
  }, []);

  useEffect(() => {
    loadDatasets();
    loadExperiments();
  }, [loadDatasets, loadExperiments]);

  const loadCases = async (dataset: Dataset) => {
    setSelectedDataset(dataset);
    setCasesLoading(true);
    try {
      const res = await harnessApi.listDatasetCases(dataset.id) as any;
      setCases(res?.cases || []);
    } catch {
      setCases([]);
    } finally {
      setCasesLoading(false);
    }
  };

  const loadResults = async (exp: Experiment) => {
    setResultsExperiment(exp);
    setResultsLoading(true);
    try {
      const res = await harnessApi.getExperimentResults(exp.id) as any;
      setResults(res?.results || []);
    } catch {
      setResults([]);
    } finally {
      setResultsLoading(false);
    }
  };

  // ==================== Dataset Actions ====================

  const handleCreateDataset = async (values: { name: string; description?: string }) => {
    try {
      await harnessApi.createDataset(values);
      message.success('数据集创建成功');
      setCreateDatasetOpen(false);
      datasetForm.resetFields();
      loadDatasets();
    } catch {
      message.error('创建失败');
    }
  };

  const handleDeleteDataset = async (id: string) => {
    try {
      await harnessApi.deleteDataset(id);
      message.success('删除成功');
      if (selectedDataset?.id === id) {
        setSelectedDataset(null);
        setCases([]);
      }
      loadDatasets();
    } catch {
      message.error('删除失败');
    }
  };

  // ==================== Case Actions ====================

  const handleAddCase = async (values: { input: string; expected: string; context?: string; tags?: string }) => {
    if (!selectedDataset) return;
    try {
      const tags = values.tags ? values.tags.split(',').map((t: string) => t.trim()).filter(Boolean) : [];
      await harnessApi.addDatasetCase(selectedDataset.id, {
        input: values.input,
        expected: values.expected,
        context: values.context || '',
        tags,
      });
      message.success('用例添加成功');
      setAddCaseOpen(false);
      caseForm.resetFields();
      loadCases(selectedDataset);
      loadDatasets();
    } catch {
      message.error('添加失败');
    }
  };

  const handleImportCases = async (values: { format: string; content: string }) => {
    if (!selectedDataset) return;
    try {
      const res = await harnessApi.importDatasetCases(selectedDataset.id, {
        format: values.format,
        content: values.content,
      }) as any;
      message.success(`成功导入 ${res?.imported || 0} 条用例`);
      setImportOpen(false);
      importForm.resetFields();
      loadCases(selectedDataset);
      loadDatasets();
    } catch {
      message.error('导入失败');
    }
  };

  // ==================== Experiment Actions ====================

  const handleRunExperiment = async (values: { name: string; dataset_id: string; config_type: string; config_ref: string; model?: string; prompt_content?: string }) => {
    try {
      await harnessApi.runExperiment(values);
      message.success('实验已启动');
      setRunExperimentOpen(false);
      experimentForm.resetFields();
      loadExperiments();
    } catch {
      message.error('实验启动失败');
    }
  };

  const handleCompare = async () => {
    if (compareIds.length < 2) {
      message.warning('请至少选择 2 个实验进行对比');
      return;
    }
    setCompareLoading(true);
    try {
      const res = await harnessApi.compareExperiments(compareIds) as any;
      setCompareResult(res || null);
    } catch {
      setCompareResult(null);
      message.error('对比失败');
    } finally {
      setCompareLoading(false);
    }
  };

  // ==================== Render Helpers ====================

  const statusBadge = (status: string) => {
    const map: Record<string, 'processing' | 'success' | 'error' | 'default'> = {
      running: 'processing', completed: 'success', failed: 'error', pending: 'default',
    };
    return <Badge status={map[status] || 'default'} text={status} />;
  };

  const scoreColor = (score: number) => {
    if (score >= 0.8) return '#52c41a';
    if (score >= 0.5) return '#faad14';
    return '#ff4d4f';
  };

  // ==================== Dataset Columns ====================

  const datasetColumns = [
    {
      title: '名称', dataIndex: 'name', key: 'name',
      render: (name: string, record: Dataset) => (
        <Button type="link" onClick={() => loadCases(record)}>{name}</Button>
      ),
    },
    { title: '描述', dataIndex: 'description', key: 'description', ellipsis: true },
    {
      title: '用例数', dataIndex: 'case_count', key: 'case_count',
      render: (n: number) => <Badge count={n} showZero color="blue" style={{ backgroundColor: '#1677ff' }} />,
    },
    {
      title: '创建时间', dataIndex: 'created_at', key: 'created_at',
      render: (t: number) => t ? new Date(t * 1000).toLocaleString() : '-',
    },
    {
      title: '操作', key: 'actions',
      render: (_: unknown, record: Dataset) => (
        <Space>
          <Button size="small" icon={<DatabaseOutlined />} onClick={() => loadCases(record)}>用例</Button>
          <Popconfirm title="确定删除该数据集？" onConfirm={() => handleDeleteDataset(record.id)}>
            <Button size="small" danger icon={<DeleteOutlined />}>删除</Button>
          </Popconfirm>
        </Space>
      ),
    },
  ];

  // ==================== Case Columns ====================

  const caseColumns = [
    {
      title: '输入', dataIndex: 'input', key: 'input', ellipsis: true,
      render: (v: string) => <Tooltip title={v}><span>{v || '-'}</span></Tooltip>,
    },
    {
      title: '期望输出', dataIndex: 'expected', key: 'expected', ellipsis: true,
      render: (v: string) => <Tooltip title={v}><span>{v || '-'}</span></Tooltip>,
    },
    {
      title: '上下文', dataIndex: 'context', key: 'context', ellipsis: true,
      render: (v: string) => v ? <Tooltip title={v}><Tag>{v.length > 15 ? v.slice(0, 15) + '...' : v}</Tag></Tooltip> : '-',
    },
    {
      title: '标签', dataIndex: 'tags', key: 'tags',
      render: (tags: string[]) => tags?.length ? tags.map((t, i) => <Tag key={i} color="blue">{t}</Tag>) : '-',
    },
  ];

  // ==================== Experiment Columns ====================

  const experimentColumns = [
    { title: '实验名称', dataIndex: 'name', key: 'name' },
    {
      title: '数据集', dataIndex: 'dataset_id', key: 'dataset_id',
      render: (id: string) => {
        const ds = datasets.find(d => d.id === id);
        return ds ? ds.name : id?.slice(0, 8) || '-';
      },
    },
    {
      title: '配置类型', dataIndex: 'config_type', key: 'config_type',
      render: (t: string) => <Tag color="purple">{t || '-'}</Tag>,
    },
    {
      title: '模型', dataIndex: 'model', key: 'model',
      render: (m: string) => m ? <Tag color="cyan">{m}</Tag> : '-',
    },
    {
      title: '状态', dataIndex: 'status', key: 'status',
      render: (s: string) => statusBadge(s),
    },
    {
      title: '平均分', dataIndex: 'avg_score', key: 'avg_score',
      render: (s: number) => s ? <span style={{ color: scoreColor(s), fontWeight: 600 }}>{(s * 100).toFixed(1)}%</span> : '-',
    },
    {
      title: '用例数', dataIndex: 'case_count', key: 'case_count',
    },
    {
      title: '操作', key: 'actions',
      render: (_: unknown, record: Experiment) => (
        <Space>
          <Button size="small" icon={<BarChartOutlined />} onClick={() => loadResults(record)}>结果</Button>
        </Space>
      ),
    },
  ];

  // ==================== Result Columns ====================

  const resultColumns = [
    {
      title: '用例ID', dataIndex: 'case_id', key: 'case_id',
      render: (id: string) => id?.slice(0, 8) || '-',
    },
    {
      title: '输出', dataIndex: 'output', key: 'output', ellipsis: true,
      render: (v: string) => <Tooltip title={v}><span>{v || '-'}</span></Tooltip>,
    },
    {
      title: '得分', dataIndex: 'score', key: 'score',
      render: (s: number) => <span style={{ color: scoreColor(s), fontWeight: 600 }}>{(s * 100).toFixed(1)}%</span>,
    },
    {
      title: '通过', dataIndex: 'passed', key: 'passed',
      render: (p: boolean) => p ? <Tag color="success">✓</Tag> : <Tag color="error">✗</Tag>,
    },
    {
      title: '延迟', dataIndex: 'latency_ms', key: 'latency_ms',
      render: (ms: number) => ms ? `${ms}ms` : '-',
    },
    {
      title: 'Tokens', dataIndex: 'tokens', key: 'tokens',
      render: (t: number) => t || '-',
    },
    {
      title: '错误', dataIndex: 'error', key: 'error',
      render: (e: string) => e ? <Tooltip title={e}><Tag color="red">{e.length > 20 ? e.slice(0, 20) + '...' : e}</Tag></Tooltip> : '-',
    },
  ];

  // ==================== Render ====================

  return (
    <div>
      <Tabs activeKey={activeTab} onChange={setActiveTab} items={[
        {
          key: 'datasets',
          label: <span><DatabaseOutlined /> 数据集</span>,
          children: (
            <div>
              <Space style={{ marginBottom: 16 }}>
                <Button type="primary" icon={<PlusOutlined />} onClick={() => setCreateDatasetOpen(true)}>
                  创建数据集
                </Button>
                <Button icon={<ReloadOutlined />} onClick={loadDatasets}>刷新</Button>
              </Space>
              <Table
                columns={datasetColumns}
                dataSource={datasets}
                rowKey="id"
                loading={datasetsLoading}
                locale={{ emptyText: <Empty description="暂无数据集，点击上方按钮创建" /> }}
              />

              {/* Cases panel */}
              {selectedDataset && (
                <Card
                  title={`数据集用例 — ${selectedDataset.name} (${cases.length}条)`}
                  style={{ marginTop: 16 }}
                  extra={
                    <Space>
                      <Button size="small" icon={<PlusOutlined />} onClick={() => setAddCaseOpen(true)}>添加用例</Button>
                      <Button size="small" icon={<ImportOutlined />} onClick={() => setImportOpen(true)}>批量导入</Button>
                      <Button size="small" onClick={() => loadCases(selectedDataset)}>刷新</Button>
                    </Space>
                  }
                >
                  <Table
                    columns={caseColumns}
                    dataSource={cases}
                    rowKey="id"
                    loading={casesLoading}
                    size="small"
                    locale={{ emptyText: <Empty description="暂无用例" /> }}
                  />
                </Card>
              )}
            </div>
          ),
        },
        {
          key: 'experiments',
          label: <span><ExperimentOutlined /> 实验</span>,
          children: (
            <div>
              <Space style={{ marginBottom: 16 }}>
                <Button type="primary" icon={<ExperimentOutlined />} onClick={() => setRunExperimentOpen(true)}>
                  运行实验
                </Button>
                <Button icon={<ReloadOutlined />} onClick={loadExperiments}>刷新</Button>
                <Button
                  icon={<BarChartOutlined />}
                  loading={compareLoading}
                  disabled={compareIds.length < 2}
                  onClick={handleCompare}
                >
                  对比 ({compareIds.length})
                </Button>
              </Space>
              <Table
                columns={experimentColumns}
                dataSource={experiments}
                rowKey="id"
                loading={experimentsLoading}
                rowSelection={{
                  selectedRowKeys: compareIds,
                  onChange: (keys) => setCompareIds(keys as string[]),
                }}
                locale={{ emptyText: <Empty description="暂无实验，点击上方按钮运行" /> }}
              />

              {/* Results panel */}
              {resultsExperiment && (
                <Card
                  title={`实验结果 — ${resultsExperiment.name}`}
                  style={{ marginTop: 16 }}
                  extra={
                    <Space>
                      <Statistic title="平均分" value={resultsExperiment.avg_score * 100} precision={1} suffix="%"
                        valueStyle={{ color: scoreColor(resultsExperiment.avg_score), fontSize: 16 }} />
                      <Statistic title="用例数" value={resultsExperiment.case_count} style={{ marginLeft: 16 }}
                        valueStyle={{ fontSize: 16 }} />
                    </Space>
                  }
                >
                  <Table
                    columns={resultColumns}
                    dataSource={results}
                    rowKey="id"
                    loading={resultsLoading}
                    size="small"
                    locale={{ emptyText: <Empty description="暂无结果" /> }}
                  />
                </Card>
              )}

              {/* Compare result */}
              {compareResult && (
                <Card title="实验对比" style={{ marginTop: 16 }}>
                  <Row gutter={16}>
                    {compareResult.experiments.map(exp => (
                      <Col span={Math.max(6, 24 / compareResult.experiments.length)} key={exp.id}>
                        <Card
                          size="small"
                          style={{
                            borderColor: exp.id === compareResult.best_experiment_id ? '#52c41a' : undefined,
                            borderWidth: exp.id === compareResult.best_experiment_id ? 2 : 1,
                          }}
                        >
                          <Statistic
                            title={exp.name}
                            value={exp.avg_score * 100}
                            precision={1}
                            suffix="%"
                            valueStyle={{ color: scoreColor(exp.avg_score) }}
                          />
                          <div style={{ marginTop: 8, fontSize: 12, color: '#999' }}>
                            通过 {exp.passed_count}/{exp.case_count}
                          </div>
                          {exp.id === compareResult.best_experiment_id && (
                            <Tag color="success" style={{ marginTop: 4 }}>最佳</Tag>
                          )}
                        </Card>
                      </Col>
                    ))}
                  </Row>
                </Card>
              )}
            </div>
          ),
        },
      ]} />

      {/* Create Dataset Modal */}
      <Modal
        title="创建数据集"
        open={createDatasetOpen}
        onCancel={() => setCreateDatasetOpen(false)}
        onOk={() => datasetForm.submit()}
      >
        <Form form={datasetForm} layout="vertical" onFinish={handleCreateDataset}>
          <Form.Item name="name" label="名称" rules={[{ required: true, message: '请输入数据集名称' }]}>
            <Input placeholder="例如：客服问答测试集" />
          </Form.Item>
          <Form.Item name="description" label="描述">
            <Input.TextArea rows={3} placeholder="数据集用途说明" />
          </Form.Item>
        </Form>
      </Modal>

      {/* Add Case Modal */}
      <Modal
        title={`添加用例 — ${selectedDataset?.name || ''}`}
        open={addCaseOpen}
        onCancel={() => setAddCaseOpen(false)}
        onOk={() => caseForm.submit()}
        width={640}
      >
        <Form form={caseForm} layout="vertical" onFinish={handleAddCase}>
          <Form.Item name="input" label="输入" rules={[{ required: true, message: '请输入测试输入' }]}>
            <Input.TextArea rows={3} placeholder="用户输入或 Prompt 变量" />
          </Form.Item>
          <Form.Item name="expected" label="期望输出" rules={[{ required: true, message: '请输入期望输出' }]}>
            <Input.TextArea rows={3} placeholder="期望的模型输出" />
          </Form.Item>
          <Form.Item name="context" label="上下文">
            <Input.TextArea rows={2} placeholder="可选：额外上下文信息" />
          </Form.Item>
          <Form.Item name="tags" label="标签（逗号分隔）">
            <Input placeholder="例如：简单, 客服, FAQ" />
          </Form.Item>
        </Form>
      </Modal>

      {/* Import Cases Modal */}
      <Modal
        title={`批量导入 — ${selectedDataset?.name || ''}`}
        open={importOpen}
        onCancel={() => setImportOpen(false)}
        onOk={() => importForm.submit()}
        width={640}
      >
        <Alert
          message="JSON 格式：数组 [{input, expected, context?, tags?}]；CSV 格式：首行 input,expected,context,tags"
          type="info"
          showIcon
          style={{ marginBottom: 16 }}
        />
        <Form form={importForm} layout="vertical" onFinish={handleImportCases} initialValues={{ format: 'json' }}>
          <Form.Item name="format" label="格式" rules={[{ required: true }]}>
            <Select options={[
              { value: 'json', label: 'JSON' },
              { value: 'csv', label: 'CSV' },
            ]} />
          </Form.Item>
          <Form.Item name="content" label="内容" rules={[{ required: true, message: '请输入导入内容' }]}>
            <Input.TextArea rows={10} placeholder='[{"input":"你好","expected":"你好！有什么可以帮您？"}]' />
          </Form.Item>
        </Form>
      </Modal>

      {/* Run Experiment Modal */}
      <Modal
        title="运行实验"
        open={runExperimentOpen}
        onCancel={() => setRunExperimentOpen(false)}
        onOk={() => experimentForm.submit()}
        width={640}
      >
        <Form form={experimentForm} layout="vertical" onFinish={handleRunExperiment} initialValues={{ config_type: 'prompt' }}>
          <Form.Item name="name" label="实验名称" rules={[{ required: true, message: '请输入实验名称' }]}>
            <Input placeholder="例如：v2 Prompt 测试" />
          </Form.Item>
          <Form.Item name="dataset_id" label="数据集" rules={[{ required: true, message: '请选择数据集' }]}>
            <Select placeholder="选择数据集" options={datasets.map(d => ({
              value: d.id, label: `${d.name} (${d.case_count}条)`,
            }))} />
          </Form.Item>
          <Form.Item name="config_type" label="配置类型" rules={[{ required: true }]}>
            <Select options={[
              { value: 'prompt', label: 'Prompt' },
              { value: 'model', label: '模型' },
              { value: 'agent', label: 'Agent' },
            ]} />
          </Form.Item>
          <Form.Item name="config_ref" label="配置引用" rules={[{ required: true, message: '请输入配置引用' }]}>
            <Input placeholder="Prompt key 或模型名称" />
          </Form.Item>
          <Form.Item name="model" label="模型（可选）">
            <Input placeholder="例如：gpt-4o-mini" />
          </Form.Item>
          <Form.Item name="prompt_content" label="Prompt 内容（可选，覆盖引用）">
            <Input.TextArea rows={4} placeholder="直接输入 Prompt 内容进行测试" />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  );
}
