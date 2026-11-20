import http from 'k6/http';
import { check, sleep } from 'k6';
import { Counter, Trend } from 'k6/metrics';

const exactHits = new Counter('cache_exact_hits');
const semanticHits = new Counter('cache_semantic_hits');
const cacheMisses = new Counter('cache_misses');
const hitLatency = new Trend('cache_hit_latency_ms', true);
const missLatency = new Trend('cache_miss_latency_ms', true);

export const options = {
    scenarios: {
        steady_ramp: {
            executor: 'ramping-vus',
            startVUs: 2,
            stages: [
                { duration: '15s', target: 10 },
                { duration: '30s', target: 25 },
                { duration: '15s', target: 0 },
            ],
            gracefulRampDown: '5s',
        },
    },
    thresholds: {
        http_req_failed: ['rate<0.02'],
        cache_hit_latency_ms: ['p(95)<50', 'p(99)<120'],
    },
};

const BASE_URL = __ENV.GATEWAY_URL || 'http://localhost:8080';
const TENANT_ID = 'bench_fintech_01';

// Pool of prompts designed to trigger exact hits, semantic hits, and occasional misses
const PROMPT_POOL = [
    'How do idempotency keys prevent duplicate charges in payment APIs?',
    'Explain how idempotency keys stop double payments in a payment service.', // semantic match
    'What is the difference between optimistic and pessimistic locking in databases?',
    'How does optimistic vs pessimistic concurrency control work in DBs?', // semantic match
    'Why is a transactional outbox pattern used with message brokers?',
    'How do you guarantee at-least-once event publishing with the outbox pattern?', // semantic match
    `Explain distributed lock fencing tokens in Redis at iteration ${Math.floor(Math.random() * 1000)}`, // mostly misses
];

export default function () {
    const idx = Math.floor(Math.random() * PROMPT_POOL.length);
    const prompt = PROMPT_POOL[idx];

    const payload = JSON.stringify({
        tenant_id: TENANT_ID,
        model: 'gemini-2.5-flash',
        prompt: prompt,
        threshold: 0.92,
    });

    const params = {
        headers: {
            'Content-Type': 'application/json',
            'X-Tenant-ID': TENANT_ID,
        },
        timeout: '30s',
    };

    const res = http.post(`${BASE_URL}/v1/generate`, payload, params);

    const ok = check(res, {
        'status is 200': (r) => r.status === 200,
        'has response_text': (r) => {
            try {
                const body = r.json();
                return Boolean(body && body.response_text);
            } catch (_) {
                return false;
            }
        },
    });

    if (ok && res.status === 200) {
        const body = res.json();
        const tier = res.headers['X-Cache-Tier'] || body.cache_tier;
        const elapsed = body.latency_ms || res.timings.duration;

        if (tier === 'EXACT_HIT') {
            exactHits.add(1);
            hitLatency.add(elapsed);
        } else if (tier === 'SEMANTIC_HIT') {
            semanticHits.add(1);
            hitLatency.add(elapsed);
        } else {
            cacheMisses.add(1);
            missLatency.add(elapsed);
        }
    }

    sleep(0.2);
}