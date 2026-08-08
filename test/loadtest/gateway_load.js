// k6 scenario: Private LLM Gateway (M-3 / AT-12, BD §20.1)
//
// 測定対象は「Gateway 追加遅延」— 推論時間・model load・顧客 network を分離する
// ため、同一 Runtime に対して (1) 直接、(2) Gateway 経由 の2回実行して差分を
// 取る運用とする(README.md 参照)。しきい値は合意値で上書きする。
//
//   k6 run -e BASE_URL=http://gw:8443 -e API_KEY=lkpgw_... \
//          -e MODEL=qwen-local -e RPS=50 -e P95_MS=200 gateway_load.js

import http from 'k6/http';
import { check } from 'k6';
import { Trend, Rate } from 'k6/metrics';

const latency = new Trend('gateway_latency_ms');
const ttft = new Trend('ttft_ms');
const errorRate = new Rate('errors');

const BASE_URL = __ENV.BASE_URL || 'http://127.0.0.1:8443';
const API_KEY = __ENV.API_KEY || '';
const MODEL = __ENV.MODEL || 'qwen-local';
const RPS = Number(__ENV.RPS || 50);
const DURATION = __ENV.DURATION || '5m';
const P95_MS = Number(__ENV.P95_MS || 200); // MVP 200 / Enterprise 100(BD §20.1)
const VUS = Number(__ENV.VUS || 100); // 同時接続の合意値(既定: tenantあたり100)
const STREAM_VUS = Number(__ENV.STREAM_VUS || 10);

export const options = {
  scenarios: {
    // 通常 RPS(streaming とは別管理、BD §20.2)
    completions: {
      executor: 'constant-arrival-rate',
      exec: 'completions',
      rate: RPS,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: VUS,
      maxVUs: VUS * 2,
    },
    // streaming 接続(TTFT 測定)
    streaming: {
      executor: 'constant-vus',
      exec: 'streaming',
      vus: STREAM_VUS,
      duration: DURATION,
    },
  },
  thresholds: {
    gateway_latency_ms: [`p(95)<${P95_MS}`],
    errors: ['rate<0.01'],
  },
};

const params = {
  headers: {
    'Content-Type': 'application/json',
    Authorization: `Bearer ${API_KEY}`,
  },
};

function payload(stream) {
  return JSON.stringify({
    model: MODEL,
    stream,
    max_tokens: 64,
    messages: [{ role: 'user', content: 'Reply with a short greeting.' }],
  });
}

export function completions() {
  const res = http.post(`${BASE_URL}/v1/chat/completions`, payload(false), params);
  // waiting = TTFB。Runtime 直接実行時の同値との差が Gateway 追加遅延
  latency.add(res.timings.waiting);
  const ok = check(res, {
    'status is 200': (r) => r.status === 200,
    'has usage': (r) => r.status === 200 && r.json('usage.total_tokens') > 0,
  });
  errorRate.add(!ok);
}

export function streaming() {
  const res = http.post(`${BASE_URL}/v1/chat/completions`, payload(true), params);
  // k6 は body を全読みするため timings.waiting が TTFT の近似
  ttft.add(res.timings.waiting);
  const ok = check(res, {
    'status is 200': (r) => r.status === 200,
    'is SSE': (r) => (r.headers['Content-Type'] || '').includes('text/event-stream'),
  });
  errorRate.add(!ok);
}
