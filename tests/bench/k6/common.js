// Общий код k6-сценариев: открытая модель нагрузки (constant-arrival-rate).
//
// Один запуск k6 = одна ступень лестницы нагрузки: прогрев (сценарий `warmup`,
// тот же RPS) и затем замер (сценарий `measure`). Лестницу и критерий остановки
// ведёт tests/bench/run.sh, поэтому здесь один RATE.
//
// Параметры (переменные окружения k6, -e):
//   BASE_URL    адрес gateway/сервиса, по умолчанию http://127.0.0.1:8080
//   EXPERIMENT  имя заранее созданного эксперимента (run.sh создаёт его сам)
//   RATE        целевой RPS ступени (запросов в секунду; для mixed — суммарно)
//   DURATION    длительность замера, по умолчанию 30s
//   WARMUP      длительность прогрева, по умолчанию 5s
//   MAX_VUS     потолок VU (при нехватке k6 считает dropped_iterations)
//   RUN_ID      префикс event_id/user_id; уникален для каждого запуска k6
//   P99_MS      порог p99 только для флага (thresholds не прерывают тест)

import http from 'k6/http';
import exec from 'k6/execution';
import { Counter } from 'k6/metrics';

export const BASE_URL = (__ENV.BASE_URL || 'http://127.0.0.1:8080').replace(/\/$/, '');
export const EXPERIMENT = __ENV.EXPERIMENT || 'bench_exp';
export const RATE = parseInt(__ENV.RATE || '200', 10);
export const DURATION = __ENV.DURATION || '30s';
export const WARMUP = __ENV.WARMUP || '5s';
const MAX_VUS = parseInt(__ENV.MAX_VUS || '4000', 10);
const RUN_ID = __ENV.RUN_ID || `r${Date.now().toString(36)}`;
const P99_MS = parseInt(__ENV.P99_MS || '250', 10);

const JSON_HEADERS = { 'Content-Type': 'application/json' };
const REQ_TIMEOUT = '10s';

// Счётчики исходов приёма событий. Раздельно для прогрева и замера: для проверки
// целостности нужен итог по ВСЕМ принятым событиям, для отчёта — только по замеру.
const mk = (phase) => ({
  ok: new Counter(`trk_ok_${phase}`),
  s429: new Counter(`trk_429_${phase}`),
  s503: new Counter(`trk_503_${phase}`),
  other: new Counter(`trk_other_${phase}`),
});
const counters = { warmup: mk('warmup'), measure: mk('measure') };

function secs(d) {
  const m = /^(\d+)(s|m)$/.exec(d);
  if (!m) throw new Error(`ожидалась длительность вида 30s/2m: ${d}`);
  return parseInt(m[1], 10) * (m[2] === 'm' ? 60 : 1);
}

function arrival(startTime, duration) {
  // Little: VU ≈ RPS * задержка. Берём запас, k6 добавит VU до MAX_VUS сам.
  const pre = Math.min(Math.max(100, Math.ceil(RATE * 0.1)), 1000, MAX_VUS);
  return {
    executor: 'constant-arrival-rate',
    rate: RATE,
    timeUnit: '1s',
    duration,
    startTime,
    preAllocatedVUs: pre,
    maxVUs: Math.max(pre, MAX_VUS),
    gracefulStop: '10s',
  };
}

// thresholds только помечают ступень (abortOnFail по умолчанию false) и заодно
// заставляют k6 выгрузить под-метрики {scenario:measure} в --summary-export.
export function buildOptions(names) {
  const t = {
    'http_req_duration{scenario:measure}': [`p(99)<${P99_MS}`],
    'http_req_failed{scenario:measure}': ['rate<0.01'],
    'dropped_iterations{scenario:measure}': ['count<1'],
    'http_reqs{scenario:measure}': ['count>=0'],
    'iterations{scenario:measure}': ['count>=0'],
  };
  for (const n of names) {
    t[`http_req_duration{scenario:measure,name:${n}}`] = [`p(99)<${P99_MS}`];
    t[`http_req_failed{scenario:measure,name:${n}}`] = ['rate<0.01'];
    t[`http_reqs{scenario:measure,name:${n}}`] = ['count>=0'];
  }
  return {
    discardResponseBodies: true,
    summaryTrendStats: ['min', 'med', 'avg', 'p(90)', 'p(95)', 'p(99)', 'max'],
    scenarios: {
      warmup: arrival('0s', WARMUP),
      measure: arrival(`${secs(WARMUP)}s`, DURATION),
    },
    thresholds: t,
    // Не создаём тысячи серий по url: имя тега задаём вручную (name:variants/tracker)
    systemTags: ['status', 'scenario', 'name', 'error_code'],
  };
}

function rnd() {
  return Math.random().toString(36).slice(2, 12);
}

export function getVariant() {
  const uid = `u${rnd()}${exec.scenario.iterationInTest}`;
  return http.get(`${BASE_URL}/api/v1/variants/${EXPERIMENT}?user_id=${uid}`, {
    tags: { name: 'variants' },
    timeout: REQ_TIMEOUT,
  });
}

export function postEvent() {
  const n = exec.scenario.iterationInTest;
  const phase = exec.scenario.name === 'measure' ? 'measure' : 'warmup';
  // Стабильный event_id (на ретраях совпал бы), уникальный user_id: каждое принятое
  // событие обязано дать ровно одну строку в хранилище.
  const body = JSON.stringify({
    event_id: `${RUN_ID}-${phase}-${n}`,
    user_id: `${RUN_ID}-u${n}-${rnd()}`,
    event_name: 'bench_conversion',
    timestamp: Date.now(),
    experiment_name: EXPERIMENT,
    variant: n % 2 === 0 ? 'control' : 'treatment',
    event_data: { value: 1, source: 'k6' },
  });
  const res = http.post(`${BASE_URL}/api/v1/tracker/events`, body, {
    headers: JSON_HEADERS,
    tags: { name: 'tracker' },
    timeout: REQ_TIMEOUT,
  });
  const c = counters[phase];
  if (res.status >= 200 && res.status < 300) c.ok.add(1);
  else if (res.status === 429) c.s429.add(1);
  else if (res.status === 503) c.s503.add(1);
  else c.other.add(1);
  return res;
}

// Проверка до нагрузки: эксперимент существует, активен и раздаёт оба варианта.
// Иначе Python-версия молча вернула бы "control" для неизвестного эксперимента.
export function sanityCheck() {
  const seen = {};
  for (let i = 0; i < 40; i++) {
    const r = http.get(`${BASE_URL}/api/v1/variants/${EXPERIMENT}?user_id=sanity${i}`, {
      responseType: 'text',
    });
    if (r.status !== 200) {
      throw new Error(`sanity: GET variants вернул ${r.status}`);
    }
    seen[r.json('variant')] = true;
  }
  if (!seen.control || !seen.treatment) {
    throw new Error(`sanity: эксперимент ${EXPERIMENT} не раздаёт оба варианта: ${Object.keys(seen)}`);
  }
}
