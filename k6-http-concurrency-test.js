import http from 'k6/http';
import { check, sleep } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';

// Custom metrics
const errorRate = new Rate('error_rate');
const reqLatency = new Trend('request_latency_ms');
const status2xx = new Counter('status_2xx');
const status4xx = new Counter('status_4xx');
const status5xx = new Counter('status_5xx');

// Environment variables configuration
const TARGET_URL = __ENV.TARGET_URL || 'http://localhost:8080/api/status';
const CUSTOM_VUS = __ENV.VUS ? parseInt(__ENV.VUS, 10) : 0;
const CUSTOM_DURATION = __ENV.DURATION || '1m';

// Dynamic Options Configuration
export const options = CUSTOM_VUS > 0
  ? {
      // Chế độ 1: Thủ công cố định (Manual Constant Concurrency)
      vus: CUSTOM_VUS,
      duration: CUSTOM_DURATION,
      thresholds: {
        error_rate: ['rate<0.05'], // Tỷ lệ lỗi dưới 5%
        http_req_duration: ['p(95)<3000'], // 95% request dưới 3s
      },
    }
  : {
      // Chế độ 2: Tự động Ramp-up đa giai đoạn (Theo tài liệu Azure Container Apps Scaling)
      stages: [
        { duration: '30s', target: 10 },  // Giai đoạn 1: 10 concurrent requests (khởi động 1 replica)
        { duration: '1m',  target: 50 },  // Giai đoạn 2: Tăng lên 50 concurrent requests (kích hoạt scale-out)
        { duration: '1m',  target: 100 }, // Giai đoạn 3: Đẩy lên 100 concurrent requests (mở rộng thêm replica)
        { duration: '1m',  target: 200 }, // Giai đoạn 4: Đẩy tải cao 200 concurrent requests (stress trần)
        { duration: '45s', target: 0 },   // Giai đoạn 5: Hạ tải về 0 (quan sát hạ tải / cooldown của ACA)
      ],
      thresholds: {
        error_rate: ['rate<0.10'], // Tỷ lệ lỗi dưới 10% khi stress cao
        http_req_duration: ['p(90)<2500'],
      },
    };

export default function () {
  const params = {
    headers: {
      'User-Agent': 'k6-load-tester/uet-cloud',
      'Accept': '*/*',
    },
    timeout: '30s',
  };

  const start = Date.now();
  const res = http.get(TARGET_URL, params);
  const latency = Date.now() - start;

  reqLatency.add(latency);

  const isOk = check(res, {
    'status is 2xx': (r) => r.status >= 200 && r.status < 300,
  });

  if (res.status >= 200 && res.status < 300) {
    status2xx.add(1);
    errorRate.add(0);
  } else if (res.status >= 400 && res.status < 500) {
    status4xx.add(1);
    errorRate.add(1);
  } else if (res.status >= 500 || res.status === 0) {
    status5xx.add(1);
    errorRate.add(1);
  } else {
    errorRate.add(1);
  }

  // Giữ kết nối liên tục nhằm duy trì số lượng concurrent requests in-flight
  // Không sleep dài để ACA HTTP scaling rule ghi nhận đúng lượng concurrent in-flight
}

export function handleSummary(data) {
  return {
    stdout: `
======================================================
  UET CLOUD - K6 LOAD TEST REPORT
======================================================
Target URL       : ${TARGET_URL}
VUs Mode         : ${CUSTOM_VUS > 0 ? `Manual (${CUSTOM_VUS} VUs, ${CUSTOM_DURATION})` : 'Auto Ramp-up (10 -> 50 -> 100 -> 200 -> 0)'}
Total Requests   : ${data.metrics.http_reqs ? data.metrics.http_reqs.values.count : 0}
Throughput (RPS) : ${data.metrics.http_reqs ? data.metrics.http_reqs.values.rate.toFixed(2) : 0} req/s
Avg Latency      : ${data.metrics.http_req_duration ? data.metrics.http_req_duration.values.avg.toFixed(2) : 0} ms
P95 Latency      : ${data.metrics.http_req_duration ? data.metrics.http_req_duration.values['p(95)'].toFixed(2) : 0} ms
HTTP 2xx (OK)    : ${data.metrics.status_2xx ? data.metrics.status_2xx.values.count : 0}
HTTP 4xx (Client): ${data.metrics.status_4xx ? data.metrics.status_4xx.values.count : 0}
HTTP 5xx (Server): ${data.metrics.status_5xx ? data.metrics.status_5xx.values.count : 0}
Error Rate       : ${data.metrics.error_rate ? (data.metrics.error_rate.values.rate * 100).toFixed(2) : 0}%
======================================================
`,
  };
}
