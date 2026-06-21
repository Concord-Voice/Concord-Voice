// k6 API benchmark script for Concord control-plane
// Install: brew install k6
// Run:     k6 run tests/benchmarks/api_benchmark.js
//
// Prerequisites:
//   - Control-plane running at BASE_URL (default: http://localhost:8080)
//   - At least one registered user (update TEST_EMAIL/TEST_CREDENTIAL below)

import http from 'k6/http';
import { sleep } from 'k6';
import { Rate, Trend } from 'k6/metrics';

// --- Configuration ---
const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const TEST_EMAIL = __ENV.TEST_EMAIL || 'benchmark@example.com';
const TEST_CREDENTIAL = __ENV.TEST_CREDENTIAL || 'BenchmarkPass123!';

// --- Custom Metrics ---
const errorRate = new Rate('errors');
const loginLatency = new Trend('login_latency');
const channelListLatency = new Trend('channel_list_latency');
const messageListLatency = new Trend('message_list_latency');
const memberListLatency = new Trend('member_list_latency');
const unreadLatency = new Trend('unread_latency');
const serverUnreadLatency = new Trend('server_unread_latency');

// --- Test Scenarios ---
export const options = {
  scenarios: {
    // Scenario 1: Sustained load — simulates normal API usage
    sustained: {
      executor: 'constant-vus',
      vus: 10,
      duration: '30s',
      exec: 'apiWorkload',
    },
    // Scenario 2: Spike — simulates burst of users logging in
    spike: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { duration: '10s', target: 50 },
        { duration: '10s', target: 50 },
        { duration: '5s', target: 0 },
      ],
      exec: 'loginSpike',
      startTime: '35s',
    },
  },
  thresholds: {
    http_req_duration: ['p(95)<500'], // 95th percentile under 500ms
    errors: ['rate<0.05'],            // Error rate under 5%
    login_latency: ['p(95)<300'],
    channel_list_latency: ['p(95)<200'],
    message_list_latency: ['p(95)<200'],
  },
};

// Helper: login and return access token
function login() {
  const res = http.post(`${BASE_URL}/api/v1/auth/login`, JSON.stringify({
    email: TEST_EMAIL,
    password: TEST_CREDENTIAL,
  }), { headers: { 'Content-Type': 'application/json' } });

  loginLatency.add(res.timings.duration);

  if (res.status !== 200) {
    errorRate.add(1);
    return null;
  }

  errorRate.add(0);
  const body = JSON.parse(res.body);
  return body.access_token;
}

// Helper: authenticated GET
function authGet(token, path, metric) {
  const res = http.get(`${BASE_URL}${path}`, {
    headers: { Authorization: `Bearer ${token}` },
  });

  if (metric) metric.add(res.timings.duration);
  errorRate.add(res.status >= 400 ? 1 : 0);
  return res;
}

// Scenario 1: Normal API workload (authenticated user browsing)
export function apiWorkload() {
  const token = login();
  if (!token) return;

  // List servers
  const serversRes = authGet(token, '/api/v1/servers');
  const servers = JSON.parse(serversRes.body || '{}').servers || [];

  if (servers.length > 0) {
    const server = servers[0];

    // List channels
    authGet(token, `/api/v1/servers/${server.id}/channels`, channelListLatency);

    // List members
    authGet(token, `/api/v1/servers/${server.id}/members`, memberListLatency);

    // Unread counts
    authGet(token, `/api/v1/servers/${server.id}/unread`, unreadLatency);

    // Get channels and read messages from first channel
    const chRes = authGet(token, `/api/v1/servers/${server.id}/channels`);
    const channels = JSON.parse(chRes.body || '{}').channels || [];
    if (channels.length > 0) {
      authGet(token, `/api/v1/channels/${channels[0].id}/messages`, messageListLatency);
    }
  }

  // Server unread status (all servers)
  authGet(token, '/api/v1/servers/unread-status', serverUnreadLatency);

  sleep(1);
}

// Scenario 2: Login spike
export function loginSpike() {
  login();
  sleep(0.5);
}
