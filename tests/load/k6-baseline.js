import http from 'k6/http';
import { check, sleep } from 'k6';
import { Rate, Trend } from 'k6/metrics';

const errorRate = new Rate('errors');
const p99Latency = new Trend('p99_latency', true);

export const options = {
    scenarios: {
        public_traffic: {
            executor: 'constant-arrival-rate',
            rate: 200,
            timeUnit: '1s',
            duration: '30s',
            preAllocatedVUs: 50,
            maxVUs: 100,
            exec: 'publicEndpoint',
        },
        authenticated_traffic: {
            executor: 'constant-arrival-rate',
            rate: 100,
            timeUnit: '1s',
            duration: '30s',
            preAllocatedVUs: 30,
            maxVUs: 60,
            exec: 'authEndpoint',
            startTime: '5s',
        },
    },
    thresholds: {
        http_req_duration: ['p(99)<500'],
        http_req_failed: ['rate<0.01'],
        errors: ['rate<0.05'],
    },
};

const BASE_URL = __ENV.GATEWAY_URL || 'http://localhost:8080';
const JWT_TOKEN = __ENV.JWT_TOKEN || '';

export function publicEndpoint() {
    const res = http.get(`${BASE_URL}/public/hello`);
    check(res, {
        'public status 200 or 429': (r) => r.status === 200 || r.status === 429,
    });
    errorRate.add(res.status >= 500);
    p99Latency.add(res.timings.duration);
    sleep(0.01);
}

export function authEndpoint() {
    const params = {
        headers: { 'Authorization': `Bearer ${JWT_TOKEN}` },
    };
    const res = http.get(`${BASE_URL}/api/users/load-test`, params);
    check(res, {
        'auth status 200 or 429': (r) => r.status === 200 || r.status === 429,
    });
    errorRate.add(res.status >= 500);
    p99Latency.add(res.timings.duration);
    sleep(0.01);
}

export function handleSummary(data) {
    return {
        'tests/load/results/summary.json': JSON.stringify(data, null, 2),
    };
}
