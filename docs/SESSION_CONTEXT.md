# Контекст сессии разработки SynaqCore

Статус кодовой базы, архитектурные решения и точка, на которой остановлена работа, чтобы продолжить без потери контекста.
**Проект заморожен как портфолио-снимок на 2026-10-25** (тег `v2-lite-freeze`, будет создан). Дальнейшая разработка
не планируется; отложенные пункты — в разделе «Известные ограничения» [plan.md](plan.md).
Актуально на 2026-10-23, ветка `main` (lite влита в `main` коммитом `cfdecc3`). Переход на Postgres как хранилище
событий по умолчанию (ADR 0003) был в рабочем дереве незакоммиченным; после коммита это перестаёт быть
«незакоммиченным» (описание ниже, где оно так названо, считать устаревшим).

---

## 1. Текущий статус (что сделано и готово)

### Ветки и версии
- **`main`** — текущая линия: Go-монолит `synaq` на один небольшой VPS (2 ГБ; профиль `scale` — от 4 ГБ).
  Решения — [ADR 0002](adr/0002-lite-go-monolith.md) и [ADR 0003](adr/0003-postgres-default-event-store.md).
- **Python-версия** (`experiment_api` + Redis, `track_api` → Redpanda → ClickHouse Kafka engine) зафиксирована
  локальным тегом `v1-python` (в `origin` ещё не отправлен). Из текущей линии её код удалён.

### Сервисы lite
- **`synaq` (`apps/synaq`, Go + Gin + pgx/sqlc + goose)**:
  - Выдача вариантов `/api/v1/variants/{name}`, `/batch`, `/active`, `/config/{name}`, `/eval-configs` из снимка в
    памяти (`internal/configcache`): обновление после записи admin API, по Postgres `LISTEN experiments_changed`,
    полное перечитывание раз в 30 с; при ошибке раздаётся прежний снимок.
  - Admin API `/api/v1/experiments/` (JWT Zitadel, `internal/auth`): создание, список, `PATCH` (409 для вариантов
    активного эксперимента), удаление. Валидация — `422` в формате FastAPI, строгие типы.
  - Трекер `/api/v1/tracker/events`: тело ≤ 64 КБ (`413`), строки ≤ 256 символов, `event_data` ≤ 16 КБ,
    `timestamp` (мс) в окне [сейчас − 7 дней, сейчас + 5 минут] (`400`); генерирует `event_id`, если его нет.
    Ограниченный буфер → батчевый INSERT в хранилище событий (общая механика `internal/sink/batch`); полон →
    `503` + `Retry-After: 1`.
  - **События по умолчанию — в `postgres-app`**, схема `events` (`EVENT_SINK=postgres`): миграция `00003_events.sql`,
    суточные партиции, которые ведёт сам synaq (`internal/eventstore`, `EVENTS_RETENTION_DAYS` = 90 по умолчанию,
    минимум 8; `EVENTS_PARTITIONS_AHEAD` = 3); дедупликация PK `(event_id, user_id, event_name, event_date)`,
    побеждает первая строка; отдельный пул pgx (`PG_EVENTS_MAX_CONNS` = 2) с `synchronous_commit=off` (при сбое
    сервера БД теряется порядка секунды принятых событий). ClickHouse-приёмник сохранён (`EVENT_SINK=clickhouse`).
  - `/health` (жив), `/readyz` (снимок загружен; healthcheck и старт gateway), `/health/track` (хранилище событий).
  - Миграции Postgres (`internal/store/migrations`) применяются при старте.
- **`analytics_api` (Python + FastAPI)**: `/api/v1/stats/`, движок статистики (bulkhead; Монте-Карло в
  `asyncio.to_thread`) и два агрегатора по `EVENTS_BACKEND`: `postgres` (по умолчанию, роль `synaq_analytics`,
  без сегментов, воронки и выбросов; в ответе `backend` и `unsupported_sections`, явный `segments=` → `400`) и
  `clickhouse` (полный отчёт, профиль `scale`).
- **Инфраструктура**: `postgres-app` (+ одноразовый `postgres-app-init`: роли `synaq_app` / `synaq_analytics`),
  `postgres-zitadel` (отдельный Postgres для IdP), Zitadel (`zitadel-dbinit` → `zitadel-setup` → `zitadel` →
  `zitadel-init`), gateway nginx, dashboard; ClickHouse — только в профиле compose `scale` (`make up-scale`).
  Сумма `mem_limit` долгоживущих сервисов по умолчанию 1696 МБ (≈ 1.7 ГБ), с `scale` ≈ 3.2 ГБ.
  Нужен Docker Compose ≥ 2.20.

### Безопасность
- SQL ClickHouse в `analytics_api`: валидируется всё, что подставляется в запрос; тексты ошибок ClickHouse только логируются.
- synaq: SQL к Postgres только параметризованный (sqlc); имя таблицы в INSERT зашито в код.
- JWT от Zitadel: RS256 + JWKS, проверка `iss` / `aud` / `exp` / `jti`, роли только из проектного claim; fail closed (`503`).
- nginx `limit_req`: `/api/v1/variants/` — 50 r/s с IP (burst 100), `/api/v1/tracker/` — 200 r/s (burst 400).
- Наружу только gateway на `127.0.0.1:8080` и `127.0.0.1:8081`; контейнеры приложений read-only, `cap_drop: ALL`.

### DX, тесты, CI
- `Makefile`: `up`, `up-scale`, `down`, `token`, `demo`, `test`, `lint`, `go-test`, `smoke`, `smoke-ch`,
  `zitadel-reset`, а также проверки `check-masterkey`, `check-db-passwords`, `check-clickhouse-password`.
- Тесты: `go test -race` для `apps/synaq` и `sdk/go`; pytest — 243 теста без стенда (`analytics_api`, оба
  агрегатора, Python SDK против golden-векторов), ещё 60 пропускаются без стенда/DSN; smoke-набор HTTP-API synaq —
  59 тестов на отдельном docker-стенде (`make smoke` — события в Postgres, `make smoke-ch` — в ClickHouse;
  `tests/contract/run.sh`, сам поднимает и сносит стенд). Интеграционные тесты на реальном Postgres пропускаются без
  `SYNAQ_TEST_DATABASE_URL` (Go) и `SYNAQ_TEST_PG_DSN` (паритетный тест Postgres-агрегатора).
- CI: ruff + pytest, gofmt + go vet + go test + go build, smoke-набор дважды (Postgres и ClickHouse), typecheck и
  сборка дашборда, сборка Docker-образов (`analytics_api`, `synaq`, `dashboard`); для Postgres-тестов в CI
  поднимается сервис `postgres:16-alpine`. Actions закреплены по SHA, Dependabot.
- Оценка диска для событий снята измерением на 500 000 синтетических событий: ≈ 415–509 байт на событие вместе с
  индексами (таблица — в [README](../README.md#сколько-места-займут-события)).

---

## 2. Стратегия
- Модель: **self-hosted open-source** (стартап клонирует и запускает у себя). Мультитенантность не нужна.
- Основной продукт для маленьких команд — стек по умолчанию (Postgres, 2 ГБ, ~100 событий/с в пике); профиль `scale`
  (ClickHouse уже есть как `make up-scale`, Kafka/Redpanda и реплики — позже) — в той же кодовой базе.
- Дорожная карта: [`docs/plan.md`](plan.md).

---

## 3. На чём остановились и что делать дальше

1. Ревью и коммит переезда на Postgres (приёмник, миграция `00003`, `eventstore`, роли `postgres-app-init`,
   Postgres-агрегатор, compose/Makefile/CI, документация) — сейчас всё в рабочем дереве.
2. Дальше по [`plan.md`](plan.md), приоритет 11: дисковый spool, реплики synaq, Caddy auto-TLS, установка одной
   командой, бэкап/восстановление, затем профиль `scale` с Kafka (фаза 3). Сегменты и воронка поверх Postgres — по
   потребности.

### Открытые вопросы
1. **Нужно действие владельца:** добавить в `infra/.env.infra` `SYNAQ_DB_PASSWORD` и `ANALYTICS_DB_PASSWORD`
   (≥ 16 символов, разные), иначе `make up` и `docker compose` не стартуют. На существующем томе роли создаст
   `postgres-app-init`. `apps/.env.apps` секретов БД больше не несёт (устаревшие значения игнорируются).
2. **Цифры про PostHog не проверены:** сравнение в README (~16 ГБ RAM для официального self-hosted, «hobby» без
   поддержки для продакшена) не сверялось с актуальной документацией PostHog в рамках этой работы.
3. **Тег `v1-python` не отправлен в `origin`** (`git push origin v1-python`) — решение за владельцем.
4. **Данные ClickHouse → Postgres не переносятся:** старые события видны только с профилем `scale`.
5. **Настройки Postgres под 448 МБ и размер диска** оценены измерением на синтетике и не проверялись под реальной
   нагрузкой; фактический RSS контейнеров не снимался.

### Открытые хвосты
1. **TLS для prod**: gateway и Zitadel работают по HTTP (`ZITADEL_EXTERNALSECURE=false`).
2. **A/B/n в аналитике**: сравниваются только контроль (`A` или `control`) и один вариант; нужны попарные сравнения
   с контролем и поправка на множественные проверки.
3. **Две реализации JWT-проверки** (`apps/synaq/internal/auth`, `apps/analytics_api/src/core/auth.py`): менять синхронно.
4. **Потеря событий без брокера**: батч, не записанный за 5 попыток, отбрасывается; плюс при `synchronous_commit=off`
   теряется порядка секунды при сбое сервера БД; нужен дисковый spool.
5. **Скриншоты** дашборда и вывода `make demo` в `docs/img/` (заглушка в README).
6. **Батч-эндпоинт трекера**: JS SDK буферизует события, но отправляет их по одному запросу.
7. **`LICENSE`**: README упоминает MIT, файла в репозитории нет.
8. **`make up` проверяет `ZITADEL_MASTERKEY`** (не образцовый, ровно 32 символа). Локальный `infra/.env.infra` с
   образцовым ключом не пройдёт: ключ Zitadel нельзя сменить на живых томах, поэтому новый ключ — только вместе со
   свежим томом `postgres-zitadel` (`make zitadel-reset` + удаление тома).
9. **`./venv` сломан**: у скриптов в `venv/bin` неверный shebang, поэтому инструменты запускаются через `python -m`.
