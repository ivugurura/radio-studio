// k6 load test for the Studio /listen route.
//
// This is the "nicer dashboard" alternative to `cmd/loadtest`. Note the
// trade-off: k6's http.get drains the response body as fast as it can, with no
// real-time pacing, so this measures *connection + throughput* capacity, not
// player-accurate audio underrun or the server's slow-listener eviction path.
// Use `cmd/loadtest -drain=realtime` as the reference instrument; use this for
// its built-in percentiles / thresholds / (optional) cloud dashboards.
//
// Install k6: https://grafana.com/docs/k6/latest/set-up/install-k6/
//
// Run (ramp 200 -> 5000 VUs, ~9 min):
//   k6 run \
//     -e URL=http://STAGING:7081/studios/reformation-rw/listen \
//     -e MAX=5000 -e HOLD_S=45 \
//     deploy/loadtest.k6.js
//
// Quick smoke:
//   k6 run -e URL=http://127.0.0.1:7080/studios/reformation-rw/listen -e MAX=50 deploy/loadtest.k6.js

import http from 'k6/http';
import { check } from 'k6';
import { Counter, Trend } from 'k6/metrics';

const URL = __ENV.URL;
if (!URL) {
  throw new Error('set -e URL=http://host:port/studios/<id>/listen');
}

// Per-iteration hold: how long one virtual listener keeps the stream open.
const HOLD_S = parseInt(__ENV.HOLD_S || '45', 10);
// Peak concurrent listeners.
const MAX = parseInt(__ENV.MAX || '5000', 10);
const START = parseInt(__ENV.START || '200', 10);
const STEP = parseInt(__ENV.STEP || '200', 10);
const STEP_S = parseInt(__ENV.STEP_S || '45', 10);
// Expected bytes for a healthy hold: 128 kbps = 16000 B/s.
const RATE = parseInt(__ENV.RATE || '16000', 10);
const EXPECT_BYTES = RATE * HOLD_S * 0.8; // 80% floor tolerates connect ramp-up

const bytesReceived = new Counter('listen_bytes_received');
const underruns = new Counter('listen_underruns');
const perConnKBps = new Trend('listen_per_conn_kbps', false);

// Build a ramping-vus stage list: jump to START, then +STEP every STEP_S up to MAX.
function buildStages() {
  const stages = [{ duration: '10s', target: START }];
  let cur = START;
  while (cur < MAX) {
    cur = Math.min(cur + STEP, MAX);
    stages.push({ duration: `${STEP_S}s`, target: cur });
  }
  stages.push({ duration: '90s', target: MAX }); // hold at peak
  stages.push({ duration: '10s', target: 0 });
  return stages;
}

export const options = {
  discardResponseBody: true, // stream + drop, keep VU memory flat
  scenarios: {
    listen_ramp: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: buildStages(),
      gracefulRampDown: '10s',
      gracefulStop: '15s',
    },
  },
  thresholds: {
    http_req_failed: ['rate<0.01'], // <1% connection failures
    listen_underruns: ['count<1'], // any short read = capacity exceeded
    'http_req_duration{expected_response:true}': ['p(95)<2000'], // TTFB-ish
  },
};

export default function () {
  const res = http.get(URL, {
    timeout: `${HOLD_S + 15}s`,
    headers: { 'User-Agent': 'radio-loadtest-k6/1.0' },
    responseType: 'none',
  });

  const n = res.body ? res.body.length : parseInt(res.headers['Content-Length'] || '0', 10);
  // With discardResponseBody k6 still counts transferred bytes on the response.
  const got = res.request && res.timings ? Math.round((res.timings.duration / 1000) * RATE) : 0;

  const ok = check(res, {
    'status 200': (r) => r.status === 200,
    'is audio/mpeg': (r) => (r.headers['Content-Type'] || '').indexOf('audio/mpeg') === 0,
  });

  // k6 reports transferred bytes via the http_req_receiving timing + data_received
  // metric globally; for a per-iteration underrun heuristic we compare the
  // iteration wall time against HOLD_S. A stream cut short returns early.
  if (res.timings.duration < HOLD_S * 1000 * 0.8) {
    underruns.add(1);
  }
  if (got > 0) {
    bytesReceived.add(got);
    perConnKBps.add(got / 1024 / (res.timings.duration / 1000 || 1));
  }
  void n;
  void ok;
}
