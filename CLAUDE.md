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

API services listen on port 8000 inside the network. `/api/v1/variants/` has an nginx `limit_req` (zone `variants`).

**Allocation** (`experiment_api/src/services/allocation.py`): `SHA-256(f"{user_id}:{experiment_name}") % 10000` → bucket, scaled against the sum of variant weights. It is stateless and must stay deterministic; `tests/test_allocation.py` pins this behaviour. Experiment configs are read from Redis first and fall back to Postgres (then re-cached). Redis has short timeouts (0.3 s, one retry; `services/cache.py`, needs redis-py 8 for `redis.asyncio.retry.Retry` / `redis.backoff.NoBackoff`) and a circuit breaker in `api/v1/client.py`: after an error Redis is skipped for 5 s.

**Auth (JWT via Zitadel)**: `src/core/auth.py` exists in both `experiment_api` and `analytics_api` (duplicated). Only RS256; keys from JWKS at `http://zitadel:8080/oauth/v2/keys` with a `Host: localhost:8081` override (Zitadel picks the instance by Host); `aud` = project_id read from `/zitadel-public/oidc.json` (written by the one-shot `zitadel-init`, `infra/zitadel/bootstrap.sh`, which shreds the IAM-owner `secrets/bootstrap.pat` after a successful run and exits 0 without it when `oidc.json` + `automation.json` exist; Zitadel itself runs as `zitadel-dbinit` (init, only place with Postgres superuser creds) → `zitadel-setup` (setup, first instance + PAT) → `zitadel` (start)); `iss` = `http://localhost:8081`; `jti` is required (rejects ID tokens). Roles come only from the `urn:zitadel:iam:org:project:<pid>:roles` claim. Fail closed: 503 if OIDC isn't configured or JWKS is unavailable. Per-route `require_roles(...)`: create/PATCH/DELETE experiments need `admin`; listing experiments and `/stats` accept `admin` or `analyst`. Variant serving and tracking are public. PATCH returns 409 when changing variants of an active experiment.

**Event pipeline (no Python consumer, "zero-worker")**: SDK/clients → `POST /api/v1/tracker/events` → Go publishes JSON to topic `tracking_events` (env `TOPIC`, `KAFKA_BROKERS`) → ClickHouse `Kafka` engine table `tracking_events_queue` → materialized view `tracking_events_mv` → `ReplacingMergeTree(timestamp)` table `tracking_events`. Schema is in `infra/clickhouse/init/init-db.sql`. Columns use `DEFAULT ''` instead of `Nullable`. Deduplication relies on `event_id` being in `ORDER BY (event_name, user_id, event_id, toDate(timestamp))`; `sdk/synaq.js` generates `event_id` when the caller doesn't supply one. If you change the event shape, update the Go handler, both ClickHouse tables, the MV, and the SDK together.

**Analytics** (`analytics_api`): `services/clickhouse_aggregator.py` builds the SQL. `goal_event` is regex-validated and segment values are escaped because they are interpolated into queries, so keep that validation when adding parameters. `services/analysis/` contains the stats engine (`statistical`, `business`, `segmentation`, `recommendations`). Monte Carlo work is run via `asyncio.to_thread`. Analytics was split out of `experiment_api` so heavy queries can't affect variant serving (bulkhead).

### Duplicated code to be aware of
`experiment_api/src/services/analysis/` and `analytics_api/src/services/analysis/` are currently identical copies, and so are the two `src/core/auth.py` files (change both together). `experiment_api` still contains `stats.py` and `clickhouse_aggregator.py`, but `main.py` no longer mounts the stats router, and the two aggregators have diverged. `tests/conftest.py` adds only `apps/experiment_api` to `sys.path`, so `src.*` imports in tests (including `test_analysis_engine.py`) resolve to the **experiment_api** copies, not to analytics_api. The exception is `tests/test_analytics_auth.py`, which loads analytics_api in isolation by swapping `src.*` out of `sys.modules` and restoring it afterwards.

### Tests
`tests/conftest.py` sets synthetic env before importing `src.*`, forces `OIDC_*` empty, and has an autouse fixture that resets the Redis breaker and the auth caches (JWKS, oidc.json) between tests. New tests depending on that state must rely on it, not bypass it. JWT helpers for tests are in `tests/_jwt_utils.py` (patch `src.core.auth.fetch_jwks`).

## Docs
- `docs/SYSTEM_DESIGN.md`: architecture spec
- `docs/clickhouse-kafka/README.md`: ingestion pipeline details
- `docs/plan.md`, `docs/SESSION_CONTEXT.md`: current roadmap and work-in-progress status
