# 🧠 Контекст сессии разработки SynaqCore

Этот документ фиксирует статус кодовой базы, архитектурные решения и конкретную точку, на которой остановлена работа, чтобы продолжить диалог без потери контекста.

---

## 📌 1. Текущий статус проекта (Что сделано и готово)

### 🏗️ Архитектура и микросервисы
- **`track_api_go` (Go + Gin + Franz-go)**: 
  - Высокопроизводительный сбор событий (до 100k+ EPS), запись в Kafka/Redpanda topic `tracking_events`.
  - Эндпоинт `/health`, запуск от non-root пользователя `appuser`.
  - Приём `event_id` для дедупликации.
- **Выделен независимый `analytics_api` (Python + FastAPI + ClickHouse)**:
  - Паттерн **Bulkhead**: тяжёлая аналитика (Монте-Карло, воронки, p-value) полностью отделена от выдачи вариантов.
  - Расчёт Монте-Карло вынесен в пул потоков через `asyncio.to_thread`.
- **`experiment_api` (Python + FastAPI + Redis + PostgreSQL)**:
  - Выдача вариантов `/api/v1/variants/{name}?user_id=...` и CRUD экспериментов (`POST` / `PATCH` / `DELETE /api/v1/experiments/...`).
  - Детерминированное хеширование (SHA-256).
  - Redis circuit breaker: таймауты 0.3 с, после ошибки Redis пропускается на 5 с, конфиг читается из PostgreSQL.
- **Zitadel (IdP)**: OIDC/OAuth2 на отдельном порту gateway `127.0.0.1:8081`; `zitadel-init` автоматически создаёт проект, роли `admin` / `analyst`, OIDC-приложение дашборда и сервисный аккаунт `synaq-automation`.
- **`dashboard` (React + Vite + Tailwind)**: SPA на `/dashboard/`, вход через Zitadel (PKCE). Старая HTML-админка удалена, `/admin` редиректит на дашборд.
- **Стриминг в ClickHouse (Zero-Worker Pipeline)**:
  - `Kafka Engine (tracking_events_queue)` $\rightarrow$ `Materialized View (tracking_events_mv)` $\rightarrow$ `ReplacingMergeTree (tracking_events)`.
  - Отказ от `Nullable` в пользу `DEFAULT ''` для оптимизации диска и SIMD-инструкций.
- **Дедупликация событий (Idempotency)**:
  - Движок: `ENGINE = ReplacingMergeTree(timestamp)`.
  - Ключ сортировки: `ORDER BY (event_name, user_id, event_id, toDate(timestamp))`.
  - В `sdk/synaq.js` добавлена автогенерация `event_id`.
- **Клиентский SDK и Демо**:
  - `sdk/synaq.js` (Zero-dependency JS/TS SDK для браузера и Node.js).
  - `sdk/demo_simulator.py` (CLI симулятор для генерации трафика в 1 команду).

### 🛡️ Безопасность (Аудит Security_Guard пройден)
- Защита от SQL-инъекций (CWE-89): regex-валидация `goal_event` и экранирование `segment_field` в ClickHouse агрегаторе.
- Аутентификация (CWE-306 / CWE-287): JWT от Zitadel, RS256 + JWKS, проверка `iss` / `aud` / `jti`, роли только из проектного claim. Fail closed (503), если OIDC не настроен. `X-Admin-Key` удалён.
- RBAC: запись — `admin`, чтение списка и статистики — `admin` или `analyst`.
- Rate limit публичного `/api/v1/variants/` в Nginx (50 r/s с IP, burst 100, `429`) — защита Postgres при падении Redis.
- CORS: `allow_credentials=False` при wildcard `*`.
- Сетевая изоляция: порты БД закрыты во внутренней сети `ab_testing_net`, Nginx привязан строго к `127.0.0.1:8080` и `127.0.0.1:8081` (Zitadel).
- CI: GitHub Actions (ruff, pytest, go vet/build, сборка образов), actions закреплены по SHA, Dependabot.
- Nginx Upstream: настроен пул `keepalive 64;` и `proxy_http_version 1.1;` (защита от исчерпания сокетов TIME_WAIT).

### 🧪 Тестирование
- Все **285 тестов успешно пройдены** (`./venv/bin/python -m pytest tests/ -q`), `ruff check apps tests sdk` — без замечаний.

---

## 🎯 2. Стратегия: Резюме + Оффер + Self-Hosted продукт для стартапов
- Модель: **Self-Hosted Open-Source** (стартап клонирует и запускает через `docker compose up -d`). Мультитенантность не нужна, так как у каждого своя БД.
- Полная документация с приоритетами сохранена в [`docs/plan.md`](file:///home/kebi/proga/Kebi/synaqcore/docs/plan.md).

---

## 📍 3. НА ЧЁМ ОСТАНОВИЛИСЬ И ЧТО ДЕЛАТЬ ДАЛЬШЕ

Все пункты дорожной карты (`docs/plan.md`, приоритеты 1–5) выполнены: Redis circuit breaker, GitHub Actions CI, аутентификация через Zitadel (JWT + RBAC) и SPA-дашборд. Встроенная HTML-админка удалена, публичная выдача вариантов ограничена rate limit в Nginx.

### Открытые хвосты (follow-ups):

1. **Шаблоны окружения** (`apps/.env.apps.example`, `infra/.env.infra.example`) нужно обновить вручную: добавить ключи `ZITADEL_*` и убрать `ADMIN_API_KEY` (агентам доступ к `.env*` закрыт).
2. **Аналитика сравнивает только 2 варианта**: статистический движок не поддерживает полноценный A/B/n (нужны попарные сравнения с контролем и поправка на множественные проверки).
3. **Дублирование кода**: `analysis/` и `core/auth.py` скопированы в `experiment_api` и `analytics_api`; в `experiment_api` остались неиспользуемые `stats.py` и `clickhouse_aggregator.py`. Вынести в общий пакет.
4. **TLS для prod**: сейчас gateway и Zitadel работают по HTTP (`ZITADEL_EXTERNALSECURE=false`); для продакшена нужны HTTPS и внешний домен.
5. **`./venv` сломан**: у скриптов в `venv/bin` неверный shebang, поэтому `pytest` / `ruff` запускаются только через `./venv/bin/python -m ...`. Пересоздать venv.
