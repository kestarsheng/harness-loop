import { useState } from 'react';
import { Card, Tabs } from 'antd';
import {
  EditOutlined, ExperimentOutlined, RocketOutlined
} from '@ant-design/icons';

import PromptListPage from './PromptListPage';
import EvalLabPanel from './EvalLabPanel';
import AutoOptimizerPanel from './AutoOptimizerPanel';

export default function PromptPage() {
  const [activeTab, setActiveTab] = useState('list');

  return (
    <div>
      <h2 style={{ marginBottom: 24 }}>Prompt 管理</h2>
      <Card>
        <Tabs
          activeKey={activeTab}
          onChange={setActiveTab}
          items={[
            {
              key: 'list',
              label: <span><EditOutlined /> Prompt 管理</span>,
              children: <PromptListPage />,
            },
            {
              key: 'eval',
              label: <span><ExperimentOutlined /> 评估实验室</span>,
              children: <EvalLabPanel />,
            },
            {
              key: 'optimizer',
              label: <span><RocketOutlined /> 自动优化</span>,
              children: <AutoOptimizerPanel />,
            },
          ]}
        />
      </Card>
    </div>
  );
}
