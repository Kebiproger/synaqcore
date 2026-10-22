# Контекст сессии разработки SynaqCore

Статус кодовой базы, архитектурные решения и точка, на которой остановлена работа, чтобы продолжить без потери контекста.
Актуально на 2026-10-22, ветка `lite` (интеграционные правки после коммита `7e1c60f` ещё не закоммичены).

---

## 1. Текущий статус (что сделано и готово)

### Ветки и версии
- **`lite`** — текущая линия: Go-монолит `synaq` на один VPS 2 vCPU / 4 ГБ. Решение — [ADR 0002](adr/0002-lite-go-monolith.md).
- **Python-версия** (`experiment_api` + Redis, `track_api` → Redpanda → ClickHouse Kafka engine) — история `main`,
  зафиксирована локальным тегом `v1-python` (`main`, `9409b1a`; в `origin` ещё не отправлен). Из `lite` её код удалён.

### Сервисы lite
- **`synaq` (`apps/synaq`, Go + Gin + pgx/sqlc + goose)**:
  - Выдача вариантов `/api/v1/variants/{name}`, `/batch`, `/active`, `/config/{name}`, `/eval-configs` из снимка в
    памяти (`internal/configcache`): обновление после записи admin API, по Postgres `LISTEN experiments_changed`,
    полное перечитывание раз в 30 с; при ошибке раздаётся прежний снимок.
  - Admin API `/api/v1/experiments/` (JWT Zitadel, `internal/auth`): создание, список, `PATCH` (409 для вариантов
    активного эксперимента), удаление. Валидация — `422` в формате FastAPI, строгие типы.
  - Трекер `/api/v1/tracker/events`: тело ≤ 64 КБ (`413`), строки ≤ 256 символов, `event_data` ≤ 16 КБ,
    `timestamp` (мс) в окне [сейчас − 7 дней, сейчас + 5 минут] (`400`); генерирует `event_id`, если его нет.
    Ограниченный буфер (10 000 событий / 64 МиБ) → батчевый INSERT в ClickHouse; полон → `503` + `Retry-After: 1`.
  - `/health` (жив), `/readyz` (снимок загружен; healthcheck и старт gateway), `/health/track` (ClickHouse).
  - Миграции Postgres (`internal/store/migrations`) применяются при старте.
- **`analytics_api` (Python + FastAPI + ClickHouse)**: `/api/v1/stats/`, ClickHouse-агрегатор и движок статистики
  (bulkhead; Монте-Карло в `asyncio.to_thread`).
- **Инфраструктура**: `postgres-app`, `postgres-zitadel` (отдельный Postgres для IdP), Zitadel
  (`zitadel-dbinit` → `zitadel-setup` → `zitadel` → `zitadel-init`), ClickHouse (только `01-tables.sql`,
  конфиг под малую память), gateway nginx, dashboard. Сумма `mem_limit` долгоживущих сервисов ~3.4 ГБ.

### Безопасность
- SQL ClickHouse в `analytics_api`: валидируется всё, что подставляется в запрос; тексты ошибок ClickHouse только логируются.
- synaq: SQL к Postgres только параметризованный (sqlc); имя таблицы в INSERT зашито в код.
- JWT от Zitadel: RS256 + JWKS, проверка `iss` / `aud` / `exp` / `jti`, роли только из проектного claim; fail closed (`503`).
- nginx `limit_req`: `/api/v1/variants/` — 50 r/s с IP (burst 100), `/api/v1/tracker/` — 200 r/s (burst 400).
- Наружу только gateway на `127.0.0.1:8080` и `127.0.0.1:8081`; контейнеры приложений read-only, `cap_drop: ALL`.

### DX, тесты, CI
- `Makefile`: `up`, `down`, `token`, `demo`, `test`, `lint`, `go-test`, `smoke`, `zitadel-reset`.
- Тесты: `go test -race` для `apps/synaq` и `sdk/go`; pytest — 164 юнит-теста (`analytics_api`, Python SDK против
  golden-векторов); smoke-набор HTTP-API synaq — 59 тестов на отдельном docker-стенде (`make smoke`,
  `tests/contract/run.sh`, сам поднимает и сносит стенд).
- CI: ruff + pytest, gofmt + go vet + go test + go build, smoke-набор, typecheck и сборка дашборда, сборка
  Docker-образов (`analytics_api`, `synaq`, `dashboard`). Actions закреплены по SHA, Dependabot.

---

## 2. Стратегия
- Модель: **self-hosted open-source** (стартап клонирует и запускает у себя). Мультитенантность не нужна.
- Lite — основной продукт для маленьких команд; профиль `scale` (Kafka/Redpanda, реплики) — в той же кодовой базе позже.
- Дорожная карта: [`docs/plan.md`](plan.md).

---

## 3. На чём остановились и что делать дальше

1. Ревью и коммит интеграционных правок `lite` (удаление `experiment_api`, тесты, CI, Makefile, документация).
2. Отправить тег `v1-python` в `origin` (`git push origin v1-python`) — решение за владельцем; тег уже создан локально.
3. Дальше по [`plan.md`](plan.md), приоритет 10: дисковый spool, `/metrics` + Grafana, Caddy auto-TLS, установка
   одной командой, бэкап/восстановление.

### Открытые хвосты
1. **TLS для prod**: gateway и Zitadel работают по HTTP (`ZITADEL_EXTERNALSECURE=false`).
2. **A/B/n в аналитике**: сравниваются только контроль (`A` или `control`) и один вариант; нужны попарные сравнения
   с контролем и поправка на множественные проверки.
3. **Две реализации JWT-проверки** (`apps/synaq/internal/auth`, `apps/analytics_api/src/core/auth.py`): менять синхронно.
4. **Потеря событий без брокера**: батч, не записанный за 5 попыток, отбрасывается; нужен дисковый spool.
5. **Скриншоты** дашборда и вывода `make demo` в `docs/img/` (заглушка в README).
6. **Батч-эндпоинт трекера**: JS SDK буферизует события, но отправляет их по одному запросу.
7. **`LICENSE`**: README упоминает MIT, файла в репозитории нет.
8. **`make up` проверяет `ZITADEL_MASTERKEY`** (не образцовый, ровно 32 символа). Локальный `infra/.env.infra` с
   образцовым ключом не пройдёт: ключ Zitadel нельзя сменить на живых томах, поэтому новый ключ — только вместе со
   свежим томом `postgres-zitadel` (`make zitadel-reset` + удаление тома).
9. **`./venv` сломан**: у скриптов в `venv/bin` неверный shebang, поэтому инструменты запускаются через `python -m`.
