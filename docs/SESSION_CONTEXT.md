# Контекст сессии разработки SynaqCore

Статус кодовой базы, архитектурные решения и точка, на которой остановлена работа, чтобы продолжить без потери контекста.
Актуально на 2026-10-20 (изменения сессии ещё не закоммичены в `main`).

---

## 1. Текущий статус (что сделано и готово)

### Сервисы
- **`track_api_go` (Go + Gin + franz-go)**:
  - Приём событий `POST /api/v1/tracker/events`, публикация в Redpanda (топик `tracking_events`, ключ `user_id`).
  - Валидация: тело ≤ 64 КБ (`413`), строковые поля ≤ 256 символов, `event_data` ≤ 16 КБ, `timestamp` (мс) в окне [сейчас − 7 дней, сейчас + 5 минут] (`400`).
  - Генерирует `event_id`, если клиент его не передал (JS SDK генерирует его ещё на клиенте).
  - Ограниченная очередь продюсера (по умолчанию 10 000 сообщений): при переполнении или недоступном брокере — `503` + `Retry-After: 1`, без блокировки.
  - Интерфейс приёмника `internal/sink`: `EVENT_SINK=kafka` (по умолчанию) или `clickhouse` — прямая батчевая запись в ClickHouse для lite (`CH_BUFFER_SIZE`, `CH_BATCH_SIZE`, `CH_FLUSH_INTERVAL`, ретраи).
  - `/health` проверяет приёмник; таймауты HTTP-сервера, graceful shutdown, non-root `appuser`.
- **`experiment_api` (Python + FastAPI + Redis + PostgreSQL)**: CRUD экспериментов (`POST` / `PATCH` / `DELETE /api/v1/experiments/...`), выдача вариантов (`/api/v1/variants/{name}`, `/batch`, `/active`, `/config/{name}`, `/eval-configs`). Redis circuit breaker: таймауты 0.3 с, после ошибки Redis пропускается 5 с, конфиг читается из PostgreSQL.
- **`analytics_api` (Python + FastAPI + ClickHouse)**: единственный владелец `/api/v1/stats/`, ClickHouse-агрегатора и движка статистики (bulkhead; Монте-Карло в `asyncio.to_thread`). Мёртвый код статистики из `experiment_api` удалён.
- **Zitadel (IdP)**: OIDC/OAuth2 на `127.0.0.1:8081`; `zitadel-init` создаёт проект, роли `admin` / `analyst`, OIDC-приложение дашборда и сервисный аккаунт `synaq-automation`.
- **`dashboard` (React + Vite + Tailwind)**: SPA на `/dashboard/`, вход через Zitadel (PKCE); `/admin` редиректит на дашборд.

### Распределение вариантов
- `bucket = int(SHA-256("{user_id}:{experiment}"), 16) % 10000`; варианты перебираются **по алфавиту**, сравнение в целых числах (`bucket * total < cumulative * 10000`).
- Исправлен баг: раньше использовался порядок ключей словаря, а Postgres JSONB его не сохраняет — после истечения TTL кэша Redis у части пользователей менялся вариант. Сервер, `sdk/python` и `sdk/go` синхронизированы, golden-векторы в `sdk/go/synaq_test.go`.

### ClickHouse
- Init разделён: `infra/clickhouse/init/01-tables.sql` (`tracking_events`, `ReplacingMergeTree(timestamp)`, `ORDER BY (event_name, user_id, event_id, toDate(timestamp))`) и `02-kafka.sql` (очередь + MV, только режим с Kafka). Старый единый init-скрипт удалён.
- **Исправлен баг Kafka-engine:** ClickHouse 24.8 отклонял `CREATE` очереди с `DEFAULT`-колонками, поэтому на свежем томе очередь и MV не создавались, а события копились в Redpanda. Теперь у очереди нет `DEFAULT`, включён `kafka_handle_error_mode = 'stream'`, MV фильтрует `length(_error) = 0`.
- **Важно для существующих установок:** init-скрипты выполняются только на пустом томе, поэтому существующий том ClickHouse сохраняет старую схему Kafka-engine. Перед демо выполнить `make ch-reset` (пересоздаёт `tracking_events_queue` и `tracking_events_mv` из `02-kafka.sql`) или `docker compose down -v` (удаляет все тома, включая Postgres и Zitadel).

### Безопасность
- SQL ClickHouse: валидируется всё, что подставляется в запрос (`goal_event`, шаги воронки, guardrail-события — regex; сегменты — regex + экранирование, максимум 10; `metric_field` — whitelist; `limit` — целое). Тексты ошибок ClickHouse только логируются.
- JWT от Zitadel: RS256 + JWKS, проверка `iss` / `aud` / `exp` / `jti`, роли только из проектного claim; fail closed (`503`). Статического admin-ключа больше нет.
- RBAC: запись — `admin`, чтение списка и статистики — `admin` или `analyst`.
- nginx `limit_req`: `/api/v1/variants/` — 50 r/s с IP (burst 100), `/api/v1/tracker/` — 200 r/s с IP (burst 400), сверх — `429`.
- Наружу только gateway на `127.0.0.1:8080` и `127.0.0.1:8081`.

### DX, тесты, CI
- `Makefile`: `up`, `down`, `token` (только в `eval`, токен не печатается в терминал), `demo`, `ch-reset`, `test`, `lint`, `go-test`.
- `sdk/demo_simulator.py` (`make demo`): создаёт эксперимент `demo_checkout_button`, 5000 синтетических пользователей, локальное распределение через `sdk/python` со сверкой с сервером, отправка событий, отчёт `/stats`.
- Тесты: **402 pytest-теста** проходят (`make test`), `ruff` без замечаний (`make lint`); Go-тесты трекера (хендлер, Kafka-продюсер, ClickHouse-приёмник) и `sdk/go` проходят (`make go-test`).
- CI (GitHub Actions): ruff + pytest; gofmt + go vet + go test для трекера и `sdk/go`, go build трекера; typecheck и сборка дашборда; сборка Docker-образов. Actions закреплены по SHA, Dependabot.

---

## 2. Стратегия
- Модель: **self-hosted open-source** (стартап клонирует и запускает у себя). Мультитенантность не нужна.
- Ветка `main` — «enterprise»-вариант. Планируется ветка `lite`: один Go-сервис (варианты + трекер + admin API), конфиги в памяти (Postgres `LISTEN/NOTIFY` + перезагрузка раз в 30 с), прямая запись в ClickHouse, без Redis и Kafka, отдельные Postgres для бизнес-данных и Zitadel, аналитика остаётся на Python. Цель — 1 VPS 2 vCPU / 4 GB, ~1–2 тыс. событий/с.
- Дорожная карта: [`docs/plan.md`](plan.md).

---

## 3. На чём остановились и что делать дальше

1. Финальное ревью и проверка безопасности всех изменений в `main`, затем коммит.
2. Создать ветку `lite` и начать Go-монолит (см. приоритет 9 в [`plan.md`](plan.md)).

### Открытые хвосты
1. **TLS для prod**: gateway и Zitadel работают по HTTP (`ZITADEL_EXTERNALSECURE=false`); нужны HTTPS и внешний домен.
2. **A/B/n в аналитике**: сравниваются только контроль (`A` или `control`) и один вариант; нужны попарные сравнения с контролем и поправка на множественные проверки (например, Холм–Бонферрони).
3. **Дублирование `src/core/auth.py`** в `experiment_api` и `analytics_api`: вынести в общий пакет.
4. **Лимит очереди Kafka-продюсера**: задокументировать `KAFKA_MAX_BUFFERED` в `apps/.env.apps.example` (агентам доступ к `.env*` закрыт — нужно вручную). Проверить, что переменная действительно читается в `cmd/api/main.go`: на момент записи там использовалась константа `DefaultMaxBuffered` = 10 000.
5. **Скриншоты** дашборда и вывода `make demo` в `docs/img/` (заглушка в README).
6. **Батч-эндпоинт трекера**: JS SDK буферизует события, но отправляет их по одному запросу.
7. **`LICENSE`**: README упоминает MIT, файла в репозитории нет.
8. **`./venv` сломан**: у скриптов в `venv/bin` неверный shebang, поэтому инструменты запускаются через `python -m` (так и делает `Makefile`). Пересоздать venv.
