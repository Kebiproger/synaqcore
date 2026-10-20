# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

SynaqCore is a server-side A/B/n experimentation platform: deterministic variant allocation, event ingestion through Kafka (Redpanda), and statistical analytics on ClickHouse. Code comments, logs, docs and README are in Russian.

## Commands

```bash
# Full stack (infra + apps + nginx gateway). Gateway: 127.0.0.1:8080 (platform) and 127.0.0.1:8081 (Zitadel).
cp apps/.env.apps.example apps/.env.apps
cp infra/.env.infra.example infra/.env.infra
# infra/.env.infra must also set: ZITADEL_MASTERKEY (exactly 32 chars, immutable after first init),
# ZITADEL_DATABASE_POSTGRES_USER_PASSWORD, ZITADEL_DATABASE_POSTGRES_ADMIN_USERNAME/_PASSWORD
# (= POSTGRES_USER/POSTGRES_PASSWORD), ZITADEL_FIRSTINSTANCE_ORG_HUMAN_USERNAME/_PASSWORD.
docker compose up -d --build

# JWT for API calls / locust (service account synaq-automation, role admin; needs curl + jq)
export SYNAQ_TOKEN="$(scripts/get_token.sh)"

# Unit tests and lint (repo root). venv/bin scripts have a broken shebang: always use `python -m`.
./venv/bin/python -m pytest tests/ -q
./venv/bin/python -m pytest tests/test_allocation.py::TestDeterministic::test_same_user_same_variant
./venv/bin/python -m ruff check apps tests sdk
# or: make test / make lint / make go-test (race) / make demo / make ch-reset (see make help)

# Load test against the gateway (/api/v1/variants/ is rate-limited to 50 r/s per IP, burst 100 -> 429)
locust -f tests/load/locustfile.py --host http://localhost:8080

# Go tracker (from apps/track_api_go)
go build ./cmd/api
```

The root `docker-compose.yml` just `include`s `infra/docker-compose.infra.yml`, `apps/docker-compose.apps.yml` and `nginx/docker-compose.yml`; all share the `ab_testing_net` network. Python services mount their source and run `uvicorn --reload`, so code edits apply without rebuilding; the Go service and the dashboard (built inside its Docker image) need `--build`. After recreating any app container, run `docker compose restart gateway`: nginx resolves upstream names only at startup and keeps the old IP. Don't create a root-level `.env`: it overrides `infra/.env.infra` in compose interpolation (that's why Zitadel uses separate `ZITADEL_*` names instead of `POSTGRES_*`).

## Architecture

Services behind Nginx (`nginx/nginx.conf` routes by path prefix):

| Path | Service | Stack |
|---|---|---|
| `/api/v1/experiments/`, `/api/v1/variants/`, `/health` | `experiment_api` | FastAPI + PostgreSQL (source of truth) + Redis (config cache) |
| `/api/v1/tracker/` , `/health/track` | `track_api` (`apps/track_api_go`) | Go + Gin + franz-go |
| `/api/v1/stats/` | `analytics_api` | FastAPI + ClickHouse |
| `/dashboard/` (`/admin` → 302 here) | `dashboard` (`apps/dashboard`) | React + Vite + Tailwind SPA, served by nginx-unprivileged on 8080 |
| separate server on 127.0.0.1:8081 | `zitadel` (IdP) | Zitadel on the shared Postgres; not under a path prefix because its root paths (`/admin/v1`, ...) would collide |

API services listen on port 8000 inside the network. nginx `limit_req`: `/api/v1/variants/` (zone `variants`, 50 r/s) and `/api/v1/tracker/` (zone `tracker`, 200 r/s, burst 400), both per IP → 429. `real_ip` trust is off by default (spoofable `CF-Connecting-IP` from the docker bridge would bypass the limits); enable it only for real proxy/Cloudflare ranges (template in the config). CORS is set **only by nginx** (apps send none; `proxy_hide_header Access-Control-*` at server level): public `*` without credentials on `/sdk/`, `/api/v1/variants/`, `/api/v1/tracker/` (nginx answers OPTIONS itself); `/api/v1/experiments/` and `/api/v1/stats/` are same-origin only (dashboard is served by the same gateway).

**Allocation** (`experiment_api/src/services/allocation.py`): `bucket = int(SHA-256(f"{user_id}:{experiment_name}"), 16) % 10000`; variants are walked in **alphabetical order** (`sorted()`; JSONB does not preserve key order, so dict order must never matter) and the first one with `bucket * total < cumulative * 10000` wins (exact integer math). It is stateless and must stay deterministic; the Python SDK (`sdk/python`) and Go SDK (`sdk/go`, golden vectors in `synaq_test.go`) implement the same rule. `tests/test_allocation.py` and `tests/test_server_sdk.py` pin this behaviour. Experiment configs are read from Redis first and fall back to Postgres (then re-cached). Redis has short timeouts (0.3 s, one retry; `services/cache.py`, needs redis-py 8 for `redis.asyncio.retry.Retry` / `redis.backoff.NoBackoff`) and a circuit breaker in `api/v1/client.py`: after an error Redis is skipped for 5 s.

**Auth (JWT via Zitadel)**: `src/core/auth.py` exists in both `experiment_api` and `analytics_api` (duplicated). Only RS256; keys from JWKS at `http://zitadel:8080/oauth/v2/keys` with a `Host: localhost:8081` override (Zitadel picks the instance by Host); `aud` = project_id read from `/zitadel-public/oidc.json` (written by the one-shot `zitadel-init`, `infra/zitadel/bootstrap.sh`, which shreds the IAM-owner `secrets/bootstrap.pat` after a successful run and exits 0 without it when `oidc.json` + `automation.json` exist; Zitadel itself runs as `zitadel-dbinit` (init, only place with Postgres superuser creds) → `zitadel-setup` (setup, first instance + PAT) → `zitadel` (start)); `iss` = `http://localhost:8081`; `jti` is required (rejects ID tokens). Roles come only from the `urn:zitadel:iam:org:project:<pid>:roles` claim. Fail closed: 503 if OIDC isn't configured or JWKS is unavailable. Per-route `require_roles(...)`: create/PATCH/DELETE experiments need `admin`; listing experiments and `/stats` accept `admin` or `analyst`. Variant serving and tracking are public. PATCH returns 409 when changing variants of an active experiment. Variant weights are ints in 1..1_000_000.

**Event pipeline (no Python consumer, "zero-worker")**: SDK/clients → `POST /api/v1/tracker/events` → Go publishes JSON to topic `tracking_events` (env `TOPIC`, `KAFKA_BROKERS`) → ClickHouse `Kafka` engine table `tracking_events_queue` → materialized view `tracking_events_mv` → `ReplacingMergeTree(timestamp)` table `tracking_events`. Schema is in `infra/clickhouse/init/`: `01-tables.sql` (the `tracking_events` table) and `02-kafka.sql` (queue table + MV, full mode only). `tracking_events` columns use `DEFAULT ''` instead of `Nullable`; the Kafka queue table must NOT have DEFAULTs (ClickHouse 24.8 rejects them; `input_format_null_as_default=1` fills `''`), uses `kafka_handle_error_mode='stream'`, and the MV filters `length(_error) = 0`. Init scripts run only on an empty volume.

**Tracker** (`apps/track_api_go`): handler depends on `internal/sink.Producer`; `EVENT_SINK=kafka` (default, `internal/kafka`) or `clickhouse` (direct batched HTTP insert, `internal/sink/clickhouse`, for the lite deployment). `TrackEvent.Validate` enforces 64 KB body (413), 256-rune string fields, 16 KB `event_data`, and timestamp (ms) within `[now-7d, now+5m]` (400) — the window protects ClickHouse from partition explosion. Buffers are bounded by count and bytes (`KAFKA_MAX_BUFFERED`/`KAFKA_MAX_BUFFERED_BYTES`, `CH_BUFFER_SIZE`/`CH_MAX_BUFFERED_BYTES`, 64 MiB default); full buffers return 503 + `Retry-After` (never block). `/health` pings the sink. Deduplication relies on `event_id` being in `ORDER BY (event_name, user_id, event_id, toDate(timestamp))`; the tracker generates a UUIDv4 when the client omits it (so callers that retry must send a stable id; `sdk/synaq.js` does). Existing ClickHouse volume after changing `02-kafka.sql`: `make ch-reset` (recreates queue + MV only). If you change the event shape, update the Go handler, both ClickHouse tables, the MV, and the SDK together.

**Analytics** (`analytics_api`): `services/clickhouse_aggregator.py` builds the SQL. Everything interpolated into SQL is validated: `goal_event`, funnel steps and guardrail events by regex, segments by regex + escaping (max 10), `metric_field` by whitelist, `limit` as int. Keep that when adding parameters. ClickHouse error bodies are logged, never returned to clients. `services/analysis/` contains the stats engine (`statistical`, `business`, `segmentation`, `recommendations`). Monte Carlo work is run via `asyncio.to_thread`. Analytics was split out of `experiment_api` so heavy queries can't affect variant serving (bulkhead).

### Duplicated code to be aware of
The two `src/core/auth.py` files (`experiment_api`, `analytics_api`) are duplicated: change both together. The stats engine (`services/analysis/`) and the ClickHouse aggregator live only in `analytics_api`. `tests/conftest.py` adds only `apps/experiment_api` to `sys.path`, so plain `src.*` imports in tests resolve to experiment_api. Tests for analytics_api (`test_analysis_engine.py`, `test_analytics_aggregator.py`, `test_analytics_auth.py`) load it in isolation via `tests/_isolated_import.py` (`load_analytics_modules(...)`), which swaps `src.*` out of `sys.modules` and restores it afterwards.

### Tests
`tests/conftest.py` sets synthetic env before importing `src.*`, forces `OIDC_*` empty, and has an autouse fixture that resets the Redis breaker and the auth caches (JWKS, oidc.json) between tests. New tests depending on that state must rely on it, not bypass it. JWT helpers for tests are in `tests/_jwt_utils.py` (patch `src.core.auth.fetch_jwks`).

## Docs
- `docs/SYSTEM_DESIGN.md`: architecture spec
- `docs/clickhouse-kafka/README.md`: ingestion pipeline details
- `docs/plan.md`, `docs/SESSION_CONTEXT.md`: current roadmap and work-in-progress status
