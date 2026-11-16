// monitor.js —— 大屏侧数据面压测（k6）。
//
// 压的是大屏的「降级轮询」路径：50 个标签页，每个 1s 刷一次事件列表，
// 对应 DFX-P-08 的 1s 刷新周期在服务端的开销。
//
// 为什么不在这里压 SSE：k6 没有原生 text/event-stream 模块（xk6-sse 需自行
// 编译扩展）。SSE 主路径（harnessloop → gateway → 大屏）由
// bench/p2/scripts/cmd/events 用 Go 实测，本脚本只覆盖轮询回退路径。
// 两条路径分开测，不混口径。
//
// 前端主线程单帧 ≤16ms 这一条**不在本脚本范围**：它依赖真实浏览器渲染，
// 需要 node_modules + headless Chrome，见 perf-suite.md 的偏差项 3。
//
// 运行：
//   k6 run bench/p2/k6/monitor.js
//   k6 run -e BASE=http://127.0.0.1:9000 -e TENANT=tenant-bench-p2t1 bench/p2/k6/monitor.js
//
// 门槛（脚本内断言）：
//   http_req_failed < 1%
//   http_req_duration p(95) < 200ms

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Trend, Rate } from 'k6/metrics';

const BASE = __ENV.BASE || 'http://127.0.0.1:9000';
const TENANT = __ENV.TENANT || 'tenant-bench-p2t1';
const LIMIT = __ENV.LIMIT || '200';

const pollLatency = new Trend('monitor_poll_latency', true);
const eventCount = new Trend('monitor_events_per_poll');
const emptyPolls = new Rate('monitor_empty_polls');

export const options = {
  scenarios: {
    // 50 个并发大屏标签页，各跑 60s，1s 一个刷新周期。
    board_50: {
      executor: 'constant-vus',
      vus: 50,
      duration: __ENV.DURATION || '60s',
    },
  },
  thresholds: {
    http_req_failed: ['rate<0.01'],
    http_req_duration: ['p(95)<200'],
    monitor_empty_polls: ['rate<0.5'],
  },
};

export default function () {
  const url = `${BASE}/api/v2/harness-loop/events?since_seq=0&limit=${LIMIT}`;
  const res = http.get(url, {
    headers: { 'X-Tenant-ID': TENANT },
    tags: { name: 'monitor_events_poll' },
    timeout: '5s',
  });

  pollLatency.add(res.timings.duration);

  const ok = check(res, {
    'status is 200': (r) => r.status === 200,
    'body has events array': (r) => {
      try {
        const body = JSON.parse(r.body);
        return Array.isArray(body.events);
      } catch (e) {
        return false;
      }
    },
  });

  if (ok) {
    try {
      const n = JSON.parse(res.body).events.length;
      eventCount.add(n);
      emptyPolls.add(n === 0);
    } catch (e) {
      emptyPolls.add(true);
    }
  }

  // 1s 刷新周期：k6 的 constant-vus 不会自动对齐，显式睡到下一周期。
  // 减去本次请求耗时，让端到端周期接近 1s 而不是 1s+RT。
  const remain = 1000 - res.timings.duration;
  if (remain > 0) {
    sleep(remain / 1000);
  }
}
