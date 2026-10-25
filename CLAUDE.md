# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

SynaqCore is a server-side A/B/n experimentation platform. The default deployment targets one VPS (2 GB RAM is enough; `scale` profile from 4 GB): a Go monolith `synaq` (variant serving from an in-memory config cache, admin API, event tracker writing batches directly to Postgres by default, ClickHouse in the `scale` profile) plus a Python `analytics_api`. No Redis, no Kafka. The previous Python version (FastAPI `experiment_api` + Redis, Go `track_api` → Redpanda → ClickHouse Kafka engine) is preserved under git tag `v1-python`; don't reintroduce its components. A heavier `scale` profile (Kafka sink) is planned on top of this codebase, not as a separate branch. Code comments, logs, docs and README are in Russian.

## Commands

```bash
# Full stack (infra + apps + nginx gateway). Gateway: 127.0.0.1:8080 (platform) and 127.0.0.1:8081 (Zitadel).
cp apps/.env.apps.example apps/.env.apps
cp infra/.env.infra.example infra/.env.infra
# infra/.env.infra: POSTGRES_* (postgres-app; same values in apps/.env.apps), CLICKHOUSE_PASSWORD,
# ZITADEL_MASTERKEY (exactly 32 chars, immutable after first init; `make up` runs `check-masterkey` and refuses
# the sample value — generate with `tr -dc A-Za-z0-9 </dev/urandom | head -c 32`), ZITADEL_DATABASE_POSTGRES_USER_PASSWORD,
# ZITADEL_DATABASE_POSTGRES_ADMIN_USERNAME/_PASSWORD (superuser of the separate postgres-zitadel),
# ZITADEL_FIRSTINSTANCE_ORG_HUMAN_USERNAME/_PASSWORD.
make up            # docker compose up -d --build
# Coming from an older checkout: docker compose down -v (deletes ALL data) && make zitadel-reset && make up

# JWT for API calls / locust (service account synaq-automation, role admin; needs curl + jq)
export SYNAQ_TOKEN="$(scripts/get_token.sh)"

# Go service (from apps/synaq)
gofmt -l . && go vet ./... && go test -race ./...
go build ./cmd/synaq

# Python unit tests and lint (repo root). venv/bin scripts have a broken shebang: always use `python -m`.
./venv/bin/python -m pytest tests/ -q          # contract tests are skipped here (need the stand)
./venv/bin/python -m ruff check apps tests sdk

# HTTP smoke suite for synaq on its own docker stand (Postgres + ClickHouse + static JWKS); tears itself down
tests/contract/run.sh -q                       # = make smoke
# or: make test / make lint / make go-test / make demo / make smoke / make zitadel-reset (see make help)

# Load test against the gateway (/api/v1/variants/ is rate-limited to 50 r/s per IP, burst 100 -> 429)
locust -f tests/load/locustfile.py --host http://localhost:8080
```

The root `docker-compose.yml` just `include`s `infra/docker-compose.infra.yml`, `apps/docker-compose.apps.yml` and `nginx/docker-compose.yml`; all share the `ab_testing_net` network. synaq, analytics_api and the dashboard run from built images (no source mounts): code changes need `docker compose up -d --build <service>`. After recreating any app container, run `docker compose restart gateway`: nginx resolves upstream names only at startup and keeps the old IP. Don't create a root-level `.env`: it overrides `infra/.env.infra` in compose interpolation. Every container has a `mem_limit` (long-running total ≈ 3.4 GB); keep the budget when adding services.

## Architecture

Services behind Nginx (`nginx/nginx.conf` routes by path prefix):

| Path | Service | Stack |
|---|---|---|
| `/api/v1/experiments/`, `/api/v1/variants/`, `/api/v1/tracker/`, `/health`, `/health/track` | `synaq` (`apps/synaq`) | Go + Gin + pgx/sqlc + goose; Postgres `postgres-app` (config + events); ClickHouse only with `EVENT_SINK=clickhouse` |
| `/api/v1/stats/` | `analytics_api` | FastAPI + Postgres (default) or ClickHouse (`EVENTS_BACKEND`) |
| `/dashboard/` (`/admin` → 302 here) | `dashboard` (`apps/dashboard`) | React + Vite + Tailwind SPA, served by nginx-unprivileged on 8080 |
| separate server on 127.0.0.1:8081 | `zitadel` (IdP) | Zitadel on its own `postgres-zitadel`; not under a path prefix because its root paths (`/admin/v1`, ...) would collide |

API services listen on port 8000 inside the network. nginx `limit_req`: `/api/v1/variants/` (zone `variants`, 50 r/s) and `/api/v1/tracker/` (zone `tracker`, 200 r/s, burst 400), both per IP → 429. `real_ip` trust is off by default (spoofable `CF-Connecting-IP` from the docker bridge would bypass the limits); enable it only for real proxy/Cloudflare ranges (template in the config). CORS is set **only by nginx** (apps send none; `proxy_hide_header Access-Control-*` at server level): public `*` without credentials on `/sdk/`, `/api/v1/variants/`, `/api/v1/tracker/` (nginx answers OPTIONS itself); `/api/v1/experiments/` and `/api/v1/stats/` are same-origin only. The gateway waits for synaq's healthcheck (`/readyz`).

**Allocation** (`apps/synaq/internal/allocation`): `bucket = int(SHA-256(f"{user_id}:{experiment_name}"), 16) % 10000`; variants are walked in **alphabetical order** (JSONB does not preserve key order, so map order must never matter) and the first one with `bucket * total < cumulative * 10000` wins (exact integer math). It is stateless and must stay deterministic; the Python SDK (`sdk/python`) and Go SDK (`sdk/go`) implement the same rule. Golden vectors computed by the Python server are committed in `apps/synaq/internal/allocation/testdata/golden.json` (used by `allocation_test.go` and `tests/test_server_sdk.py`; `sdk/go/synaq_test.go` has its own copy).

**Config cache** (`internal/configcache`): immutable snapshot swapped atomically; the hot path is one `atomic.Load`, no DB. Refreshed after admin writes (read-your-writes), on Postgres `LISTEN experiments_changed` (trigger in migration `00002_experiments_notify.sql`, 50 ms debounce), every 30 s, and after listener reconnect. On reload errors the previous snapshot keeps serving. `/readyz` = first snapshot loaded. Migrations (`internal/store/migrations`, goose, embedded) run at startup; queries are sqlc-generated (`internal/store/queries` → `sqlcdb`).

**Admin API** (`internal/api/admin`, `internal/api/validate`): HTTP contract (paths, JSON, codes, `detail` texts) matches the Python version; `422` in FastAPI format `{"detail":[{"type","loc","msg"}]}` but with strict types (no lax coercion). PATCH returns 409 when changing variants of an active experiment. Variant weights are ints in 1..1_000_000. The smoke suite `tests/contract` pins this contract.

**Auth (JWT via Zitadel)**: same logic in `apps/synaq/internal/auth` (Go) and `apps/analytics_api/src/core/auth.py` (Python) — change both together. Only RS256; keys from JWKS at `http://zitadel:8080/oauth/v2/keys` with a `Host: localhost:8081` override (Zitadel picks the instance by Host); `aud` = project_id read from `/zitadel-public/oidc.json` (written by the one-shot `zitadel-init`, `infra/zitadel/bootstrap.sh`, which shreds the IAM-owner `secrets/bootstrap.pat` after a successful run and exits 0 without it when `oidc.json` + `automation.json` exist; Zitadel itself runs as `zitadel-dbinit` (init, only place with Postgres superuser creds) → `zitadel-setup` (setup, first instance + PAT) → `zitadel` (start)); `iss` = `http://localhost:8081`; `jti` is required (rejects ID tokens). Roles come only from the `urn:zitadel:iam:org:project:<pid>:roles` claim. Fail closed: 503 if OIDC isn't configured or JWKS is unavailable. Create/PATCH/DELETE experiments need `admin`; listing experiments and `/stats` accept `admin` or `analyst`. Variant serving and tracking are public.

**Event pipeline (lite, no broker)**: SDK/clients → `POST /api/v1/tracker/events` → synaq validates (`internal/track`) → bounded in-memory buffer → batched insert via the shared `internal/sink/batch`. `EVENT_SINK=postgres` (default, see `docs/adr/0003-postgres-default-event-store.md`): table `events` with daily partitions maintained by synaq itself (`internal/eventstore`, `EVENTS_RETENTION_DAYS` 90, `EVENTS_PARTITIONS_AHEAD` 3), PK `(event_id, user_id, event_name, event_date)` dedups (first row wins), separate pgx pool (`PG_EVENTS_MAX_CONNS` 2) with `synchronous_commit=off`, migration `00003_events.sql`, roles `synaq_app` / `synaq_analytics` created by `postgres-app-init`. `EVENT_SINK=clickhouse` (`make up-scale`, compose profile `scale`; `internal/sink/clickhouse`, schema only in `infra/clickhouse/init/01-tables.sql`, init scripts run only on an empty volume). Full buffer → 503 + `Retry-After` (never blocks); a batch is retried up to 5 times, then dropped and logged. Validation: 64 KB body (413), 256-rune string fields, 16 KB `event_data`, timestamp (ms) within `[now-7d, now+5m]` (400). The tracker generates a UUIDv4 `event_id` when missing (retrying callers must send a stable id; `sdk/synaq.js` does). If you change the event shape, update the Go handler, both stores and the SDK together. Kafka returns later as part of `scale` (see `docs/adr/0002-lite-go-monolith.md`).

**Analytics** (`analytics_api`): `services/events_backend.py` picks `postgres_aggregator.py` (default; no segments/funnel/outliers, `unsupported_sections` in the response) or `clickhouse_aggregator.py` by `EVENTS_BACKEND`. Everything interpolated into SQL is validated: experiment name, `goal_event`, funnel steps and guardrail events by regex, segments by regex + escaping (max 10), `metric_field` by whitelist, `limit` as int. Keep that when adding parameters. ClickHouse error bodies are logged, never returned to clients. `services/analysis/` contains the stats engine (`statistical`, `business`, `segmentation`, `recommendations`). Monte Carlo work runs via `asyncio.to_thread`.

### Tests
`tests/conftest.py` only sets synthetic env (ClickHouse, empty `OIDC_*`) before imports. analytics_api modules are loaded in isolation via `tests/_isolated_import.py` (`load_analytics_modules(...)`), which swaps `src.*` out of `sys.modules` and restores it afterwards. JWT helpers for tests are in `tests/_jwt_utils.py`. `tests/contract/` is the synaq HTTP smoke suite; it is skipped in plain pytest and runs via `tests/contract/run.sh` (also a CI job). Go tests live next to the code in `apps/synaq`; Postgres-backed integration tests skip unless `SYNAQ_TEST_DATABASE_URL` is set.

## Docs
- `docs/SYSTEM_DESIGN.md`: architecture spec (lite)
- `docs/adr/0002-lite-go-monolith.md`: why the Go monolith; `docs/adr/0001-kafka-vs-rabbitmq.md`: Kafka (v1-python / future `scale`)
- `docs/clickhouse-kafka/README.md`: ingestion (lite direct insert; Kafka pattern for `scale`)
- `docs/plan.md`, `docs/SESSION_CONTEXT.md`: current roadmap and work-in-progress status
