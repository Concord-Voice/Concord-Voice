// k6 WebSocket benchmark script for Concord control-plane
// Install: brew install k6
// Run:     k6 run tests/benchmarks/websocket_benchmark.js
//
// Tests concurrent WebSocket connections and message throughput.

import http from 'k6/http';
import ws from 'k6/ws';
import { check, sleep } from 'k6';
import { Rate, Trend, Counter } from 'k6/metrics';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const WS_URL = __ENV.WS_URL || 'ws://localhost:8080/ws';
const TEST_EMAIL = __ENV.TEST_EMAIL || 'benchmark@example.com';
const TEST_CREDENTIAL = __ENV.TEST_CREDENTIAL || 'BenchmarkPass123!';

const wsConnectLatency = new Trend('ws_connect_latency');
const wsMessageLatency = new Trend('ws_message_latency');
const wsErrors = new Rate('ws_errors');
const wsMessagesReceived = new Counter('ws_messages_received');

export const options = {
  scenarios: {
    // Scenario 1: Concurrent WebSocket connections
    connections: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { duration: '10s', target: 50 },   // Ramp to 50 connections
        { duration: '30s', target: 50 },   // Hold at 50
        { duration: '10s', target: 100 },  // Ramp to 100
        { duration: '30s', target: 100 },  // Hold at 100
        { duration: '10s', target: 0 },    // Ramp down
      ],
      exec: 'wsConnection',
    },
  },
  thresholds: {
    ws_connect_latency: ['p(95)<1000'],  // WebSocket connect under 1s
    ws_errors: ['rate<0.1'],
  },
};

function getWSTicket(token) {
  const res = http.post(`${BASE_URL}/api/v1/auth/ws-ticket`, null, {
    headers: { Authorization: `Bearer ${token}` },
  });
  if (res.status !== 200) return null;
  return JSON.parse(res.body).ticket;
}

export function wsConnection() {
  // Login to get token
  const loginRes = http.post(`${BASE_URL}/api/v1/auth/login`, JSON.stringify({
    email: TEST_EMAIL,
    password: TEST_CREDENTIAL,
  }), { headers: { 'Content-Type': 'application/json' } });

  if (loginRes.status !== 200) {
    wsErrors.add(1);
    return;
  }

  const token = JSON.parse(loginRes.body).access_token;
  const ticket = getWSTicket(token);
  if (!ticket) {
    wsErrors.add(1);
    return;
  }

  const connectStart = Date.now();

  const res = ws.connect(`${WS_URL}?ticket=${ticket}`, null, function (socket) {
    wsConnectLatency.add(Date.now() - connectStart);
    wsErrors.add(0);

    socket.on('message', (data) => {
      wsMessagesReceived.add(1);
      try {
        const msg = JSON.parse(data);
        // Track latency for specific message types
        if (msg.type === 'presence_snapshot' || msg.type === 'server_online_counts') {
          wsMessageLatency.add(Date.now() - connectStart);
        }
      } catch {
        // Non-JSON message
      }
    });

    socket.on('error', () => {
      wsErrors.add(1);
    });

    // Keep connection alive for the scenario duration
    sleep(30);
    socket.close();
  });

  check(res, { 'ws connected': (r) => r && r.status === 101 });
}
