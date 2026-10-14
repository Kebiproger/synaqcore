<div align="center">
  <h1>🧪 SynaqCore</h1>
  <p><b>Высокопроизводительная распределённая платформа для серверных A/B/n-экспериментов<br>
  с детерминированным распределением, нативным Kafka-стримингом и OLAP-аналитикой в реальном времени</b></p>

  <!-- Badges -->
  <p>
    <img src="https://img.shields.io/badge/Go-1.22+-00ADD8?style=for-the-badge&logo=go&logoColor=white" alt="Go">
    <img src="https://img.shields.io/badge/Python-3.11+-3776AB?style=for-the-badge&logo=python&logoColor=white" alt="Python 3.11+">
    <img src="https://img.shields.io/badge/FastAPI-009688?style=for-the-badge&logo=fastapi&logoColor=white" alt="FastAPI">
    <img src="https://img.shields.io/badge/Redpanda/Kafka-FA233B?style=for-the-badge&logo=redpanda&logoColor=white" alt="Redpanda">
    <img src="https://img.shields.io/badge/ClickHouse-FFCC00?style=for-the-badge&logo=clickhouse&logoColor=black" alt="ClickHouse">
    <img src="https://img.shields.io/badge/PostgreSQL-4169E1?style=for-the-badge&logo=postgresql&logoColor=white" alt="PostgreSQL">
    <img src="https://img.shields.io/badge/Redis-DC382D?style=for-the-badge&logo=redis&logoColor=white" alt="Redis">
    <img src="https://img.shields.io/badge/Nginx-009639?style=for-the-badge&logo=nginx&logoColor=white" alt="Nginx">
    <img src="https://img.shields.io/badge/Docker-2496ED?style=for-the-badge&logo=docker&logoColor=white" alt="Docker">
  </p>

  <p>
    <img src="https://img.shields.io/github/license/Kebiproger/synaqcore?style=flat-square" alt="License">
    <img src="https://img.shields.io/badge/status-production%20ready-2ea44f?style=flat-square" alt="Status">
  </p>
</div>

---

## 📋 Содержание

- [Архитектура системы](#-архитектура-системы)
- [Ключевые преимущества](#-ключевые-преимущества)
- [Технологический стек](#-технологический-стек)
- [Быстрый старт](#-быстрый-старт)
- [Аутентификация и роли](#-аутентификация-и-роли)
- [API-интерфейсы](#-api-интерфейсы)
- [Интеграция в сервисы компании](#-интеграция-в-сервисы-компании)
- [Аналитика и математика](#-аналитика-и-математика)
- [Схема данных и стриминг ClickHouse](#-схема-данных-и-стриминг-clickhouse)
- [Структура репозитория](#-структура-репозитория)
- [Тестирование и бенчмарки](#-тестирование-и-бенчмарки)

---

## 🏗 Архитектура системы

Платформа спроектирована по событийно-ориентированной архитектуре (EDA) с полным разделением путей чтения (Variant Allocation) и записи (High-Throughput Event Ingestion):

```
                                      ┌─────────────────────────────────────┐
                                      │         API Gateway (Nginx)         │
                                      │              :8080                  │
                                      └──────────────┬──────────────────────┘
                                                     │
                           ┌─────────────────────────┴─────────────────────────┐
                           │                                                   │
     /api/v1/experiments, /variants, /stats, /dashboard               /api/v1/tracker/events
                           │                                                   │
                           ▼                                                   ▼
              ┌─────────────────────────┐                         ┌─────────────────────────┐
              │     Experiment API      │                         │     Track API (Go)      │
              │   (FastAPI / Python)    │                         │  (Gin + twmb/franz-go)  │
              └───────┬──────────┬──────┘                         └────────────┬────────────┘
                      │          │                                             │
                      ▼          ▼                                             │ Produce events
         ┌────────────────┐ ┌────────────────┐                                 ▼
         │     Redis      │ │   PostgreSQL   │                    ┌─────────────────────────┐
         │ (кэш вариантов)│ │ (эксперименты) │                    │     Redpanda (Kafka)    │
         └────────────────┘ └────────────────┘                    │  topic: tracking_events │
                                                                  └────────────┬────────────┘
                                                                               │
                                                                               │ Native Consumer
                                                                               ▼ (Kafka Engine)
                                                                  ┌─────────────────────────┐
                                                                  │  tracking_events_queue  │
                                                                  │   (ClickHouse Engine)   │
                                                                  └────────────┬────────────┘
                                                                               │
                                                                               │ Materialized View
                                                                               ▼ (tracking_events_mv)
                                                                  ┌─────────────────────────┐
                                                                  │     tracking_events     │
                                                                  │  (ClickHouse MergeTree) │
                                                                  └─────────────────────────┘
```

---

## ✨ Ключевые преимущества

### 1. 🚀 Zero-Worker Нативный Ingestion
* **Никаких промежуточных Python-воркеров:** События из Redpanda/Kafka вычитываются ClickHouse напрямую через движок `ENGINE = Kafka` и `MATERIALIZED VIEW`.
* Исключены GC-паузы, утечки памяти и лаги консьюмеров, характерные для скриптовых очередей.
* Гарантированная пакетная запись в таблицу `MergeTree` с сортировкой по `(event_name, toDate(timestamp), user_id)`.

### 2. ⚡ Высокопроизводительный Go Ingestion Core
* Сервис `track_api_go` реализован на **Go** с использованием высокопроизводительной Kafka-библиотеки `twmb/franz-go`.
* Асинхронная неблокирующая отправка батчей в Redpanda, поддержка пулинга соединений и валидация входящих JSON payload с sub-millisecond задержкой.

### 3. 🎯 Детерминированное распределение вариантов
* Пользователь с одним и тем же `user_id` всегда детерминированно попадает в один и тот же вариант эксперимента без сохранения истории распределения в базе данных:

```python
# Детерминированный бакетинг: SHA-256(user_id + experiment_name) -> [0.00 .. 99.99]
hash_hex = hashlib.sha256(f"{user_id}:{experiment_name}".encode()).hexdigest()
bucket = (int(hash_hex, 16) % 10000) / 100.0
```

* Горячий кэш конфигов и весов в Redis обеспечивает ответ на выдачу варианта за `< 2 ms`.

### 4. 🧠 Двухуровневая аналитика (Smart + Raw)
* **Smart Analytics (`/api/v1/stats/{exp}`):**
  * Частотный анализ: Z-test, p-value, 95% доверительные интервалы, относительный uplift.
  * Байесовский анализ: симуляция Монте-Карло, вероятность P(B > A), Expected Loss.
  * Сегментация по устройствам, когортам пользователей, источникам трафика.
  * Пошаговая воронка конверсий и автоматические бизнес-рекомендации на русском языке.
* **Basic Analytics (`/api/v1/stats/{exp}/basic`):**
  * Мгновенная агрегированная сводка, конверсия `page_view → cta_click`, распределение типов событий и лог последних 20 событий.

---

## 🛠 Технологический стек

| Компонент | Технология | Роль | Внутренний порт |
|-----------|-----------|------|-----------------|
| **Gateway** | Nginx Alpine | Единая точка входа, reverse proxy, проксирование заголовков | `8080` (хост) |
| **Track API** | Go 1.22, Gin, Franz-go | High-throughput приём телеметрии и публикация в Kafka | `8000` |
| **Message Broker** | Redpanda v26 (Kafka API) | Отказоустойчивый топик событий `tracking_events` (4 партиции) | `9092` |
| **OLAP Storage** | ClickHouse 24.8 | Нативная потоковая агрегация, Materialized Views, аналитика | `8123`, `9000` |
| **Experiment API** | Python 3.11, FastAPI | Управление тестами, выдача вариантов, аналитические расчёты | `8000` |
| **Analytics API** | Python 3.11, FastAPI | Статистика экспериментов (изолирован от выдачи вариантов) | `8000` |
| **Dashboard** | React, Vite, Tailwind CSS | SPA управления тестами и просмотра результатов (вход через Zitadel) | `/dashboard/` |
| **Identity Provider** | Zitadel | OIDC/OAuth2: вход в дашборд, JWT для API, роли `admin` / `analyst` | `8081` (хост) |
| **Metadata DB** | PostgreSQL 16 | Персистентное хранение экспериментов и весов вариантов | `5432` |
| **Cache DB** | Redis 7.2 | In-memory кэширование активных конфигураций | `6379` |

---

## 🚀 Быстрый старт

### Требования
* Docker Engine 24+ & Docker Compose v2+
* Git

### 1. Клонирование репозитория
```bash
git clone https://github.com/Kebiproger/synaqcore.git
cd synaqcore
```

### 2. Подготовка переменных окружения
Скопируйте шаблоны конфигураций:
```bash
cp apps/.env.apps.example apps/.env.apps
cp infra/.env.infra.example infra/.env.infra
```

В `infra/.env.infra` обязательно задайте ключи Zitadel (без них `docker compose` не стартует):

| Ключ | Значение |
|------|----------|
| `ZITADEL_MASTERKEY` | Случайная строка **ровно из 32 символов**. Шифрует данные Zitadel — после первой инициализации её нельзя изменить. |
| `ZITADEL_DATABASE_POSTGRES_USER_PASSWORD` | Пароль пользователя `zitadel` в PostgreSQL (создаётся Zitadel автоматически). |
| `ZITADEL_DATABASE_POSTGRES_ADMIN_USERNAME` | То же значение, что `POSTGRES_USER`. |
| `ZITADEL_DATABASE_POSTGRES_ADMIN_PASSWORD` | То же значение, что `POSTGRES_PASSWORD`. |
| `ZITADEL_FIRSTINSTANCE_ORG_HUMAN_USERNAME` | Логин первого администратора (например, `zitadel-admin`). |
| `ZITADEL_FIRSTINSTANCE_ORG_HUMAN_PASSWORD` | Начальный пароль администратора (будет сменён при первом входе). |
| `ZITADEL_BOOTSTRAP_PAT_EXPIRATION` | Срок действия bootstrap-PAT (ISO-8601, ≈ сейчас + 30 дней): `date -u -d '+30 days' +%Y-%m-%dT00:00:00Z`. Учитывается только при самой первой инициализации Zitadel. |

Необязательные: `ZITADEL_APP_DEVMODE` (по умолчанию `true` — разрешает `http://` redirect URI; для прода с HTTPS — `false`), `GATEWAY_PORT` / `ZITADEL_PORT` (по умолчанию `8080` / `8081`; из них же строятся redirect URI, issuer и `ZITADEL_EXTERNALPORT`).

Сгенерировать мастер-ключ можно так: `openssl rand -hex 16`.

> ⚠️ **Не создавайте `.env` в корне репозитория.** Корневой `.env` имеет приоритет над `infra/.env.infra` при интерполяции `${...}` в compose и молча переопределит `POSTGRES_*`. Поэтому для Zitadel используются отдельные имена `ZITADEL_DATABASE_POSTGRES_ADMIN_*`, а не `POSTGRES_*` напрямую.

### 3. Запуск всех сервисов платформы
Запуск всей инфраструктуры и приложений выполняется единой командой:
```bash
docker compose up -d --build
```

Zitadel стартует в три шага: `zitadel-dbinit` (`zitadel init`, единственный контейнер с учёткой суперпользователя PostgreSQL) → `zitadel-setup` (`zitadel setup`: миграции, первый инстанс, bootstrap-PAT) → `zitadel` (`zitadel start`, только непривилегированный пользователь БД). Одноразовые шаги идемпотентны и повторяются при каждом `docker compose up`.

При первом запуске одноразовый контейнер `zitadel-init` создаёт в Zitadel проект, роли `admin` / `analyst`, OIDC-приложение дашборда и сервисный аккаунт `synaq-automation`. Публичная OIDC-конфигурация записывается в `infra/zitadel/public/oidc.json`, секрет сервисного аккаунта — в `infra/zitadel/secrets/automation.json` (не коммитится). После успешного bootstrap PAT (`secrets/bootstrap.pat`, права IAM owner) удаляется; повторные запуски `zitadel-init` без PAT ничего не меняют и завершаются с кодом 0. Чтобы применить изменённые настройки OIDC-приложения позже, создайте в консоли Zitadel короткоживущий PAT для `zitadel-bootstrap`, сохраните в `infra/zitadel/secrets/bootstrap.pat` (`chmod 600`) и перезапустите `zitadel-init`.

Все внутренние порты СУБД изолированы во внутренней docker-сети `ab_testing_net`. Наружу смотрит только Gateway: `127.0.0.1:8080` (платформа) и `127.0.0.1:8081` (Zitadel).

### 4. Доступ к интерфейсам
* 🌐 **Дашборд:** [http://localhost:8080/dashboard/](http://localhost:8080/dashboard/). Вход: `<ZITADEL_FIRSTINSTANCE_ORG_HUMAN_USERNAME>@synaqcore.localhost` (по умолчанию `zitadel-admin@synaqcore.localhost`) и пароль из `.env.infra`. При первом входе Zitadel потребует сменить пароль. Старый адрес `/admin` перенаправляет сюда.
* 🔐 **Консоль Zitadel:** [http://localhost:8081/ui/console](http://localhost:8081/ui/console) (пользователи, роли, приложения).
* 🧪 **Клиентское демо-приложение:** [http://localhost:5000](http://localhost:5000) (если запущен `company_app`)

---

## 🔐 Аутентификация и роли

Управление экспериментами и статистика защищены JWT (OIDC, IdP — Zitadel). Публичные эндпоинты выдачи вариантов (`/api/v1/variants/...`) и трекинга (`/api/v1/tracker/events`) токена не требуют.

| Роль | Права |
|------|-------|
| `admin` | Создание, изменение (`PATCH`), остановка и удаление экспериментов; просмотр списка и статистики. |
| `analyst` | Только чтение: список экспериментов и статистика. |

Первый администратор и сервисный аккаунт `synaq-automation` получают роль `admin` автоматически. Чтобы выдать роль `analyst` другому пользователю: консоль Zitadel → **Projects** → проект `synaqcore` → **Authorizations** → **New** → выберите пользователя и роль `analyst`. Роль появится в токене после повторного входа.

**Токен для API и скриптов.** `scripts/get_token.sh` получает access token сервисного аккаунта (OAuth2 `client_credentials`, нужны `curl` и `jq`):
```bash
export SYNAQ_TOKEN="$(scripts/get_token.sh)"
curl -H "Authorization: Bearer $SYNAQ_TOKEN" http://localhost:8080/api/v1/experiments/
```

---

## 📡 API-интерфейсы

### 1. Выдача варианта пользователю
Детерминированное распределение по алгоритму SHA-256 с кэшированием в Redis.

```bash
curl -X GET "http://localhost:8080/api/v1/variants/landing_button_test?user_id=usr_777"
```

**Ответ:**
```json
{
  "experiment": "landing_button_test",
  "variant": "B",
  "user_id": "usr_777",
  "config": {
    "cta_text": "Купить в 1 клик",
    "cta_color": "#0f9d58"
  }
}
```

### 2. Создание эксперимента (роль `admin`)
```bash
curl -X POST "http://localhost:8080/api/v1/experiments/" \
  -H "Authorization: Bearer $SYNAQ_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "landing_button_test", "variants": {"A": 50, "B": 50}}'
```

Частичное обновление — `PATCH /api/v1/experiments/{name}`. Менять варианты у активного эксперимента нельзя (`409 Conflict`): сначала остановите его (`{"is_active": false}`).

### 3. Список активных экспериментов
```bash
curl -X GET "http://localhost:8080/api/v1/variants/active"
```

### 4. Отправка аналитического события (Go Tracker)
Событие валидируется и немедленно отправляется в Redpanda.

```bash
curl -X POST "http://localhost:8080/api/v1/tracker/events" \
  -H "Content-Type: application/json" \
  -d '{
    "user_id": "usr_777",
    "event_name": "cta_click",
    "experiment_name": "landing_button_test",
    "variant": "B",
    "event_data": "{\"screen\":\"main\",\"dwell_time_ms\":1420}"
  }'
```

**Ответ:**
```json
{"status":"OK"}
```

### 5. Умная аналитика A/B теста (роль `admin` или `analyst`)
```bash
curl -X GET "http://localhost:8080/api/v1/stats/landing_button_test?goal_event=cta_click&method=both" \
  -H "Authorization: Bearer $SYNAQ_TOKEN"
```

### 6. Базовая сводка по эксперименту
```bash
curl -X GET "http://localhost:8080/api/v1/stats/landing_button_test/basic" \
  -H "Authorization: Bearer $SYNAQ_TOKEN"
```

---

## 🔌 Интеграция в сервисы компании

SynaqCore спроектирован для бесшовного внедрения во фронтенд, бэкенд и сетевую инфраструктуру компании с минимальным TTM (Time to Market):

* 📖 **[Полное руководство по интеграции (INTEGRATION_GUIDE.md)](file:///home/kebi/proga/Kebi/synaqcore/docs/INTEGRATION_GUIDE.md)** — исчерпывающая документация для инженеров с разбором сетевой изоляции, отказоустойчивости и примерами на всех слоях стека.
* 📦 **[JavaScript / TypeScript SDK (@synaq/client)](file:///home/kebi/proga/Kebi/synaqcore/sdk/README.md)**:
  * Zero-dependency клиент весом менее 3 КБ с поддержкой TypeScript ([`sdk/synaq.d.ts`](file:///home/kebi/proga/Kebi/synaqcore/sdk/synaq.d.ts)).
  * Раздаётся напрямую со шлюза: `<script src="https://ab.company.com/sdk/synaq.js"></script>` или через npm: `npm install @synaq/client`.
  * Двухуровневое кэширование, пакетная выдача вариантов `getVariantsBatch(...)` и гарантированная доставка кликов `track(..., { useBeacon: true })`.
* 🛡️ **[Конфигурация корпоративного Nginx (company-edge-integration.example.conf)](file:///home/kebi/proga/Kebi/synaqcore/nginx/company-edge-integration.example.conf)**:
  * Готовые шаблоны для DevOps-инженеров: подключение через отдельный поддомен (`ab.company.com`) или путь (`company.com/synaq/`).
  * Полная совместимость с Cloudflare (`CF-Connecting-IP`, `set_real_ip_from`).
  * Защита дашборда управления через офисный VPN / Cloudflare Access.
* ⚙️ **Backend-to-Backend интеграция**:
  * Готовые примеры кода на **Python**, **Go** и **Node.js** для серверного вызова `GET /api/v1/variants/{name}` и отправки закрытых финансовых конверсий через `POST /api/v1/tracker/events`.

---

## 📊 Аналитика и математика

Платформа рассчитывает комплекс статистических метрик в реальном времени:

```
                          ClickHouse
                    (Агрегаты за миллисекунды)
                                │
                                ▼
                   ┌──────────────────────────┐
                   │  Statistical Analyzers   │
                   └──────┬────────────┬──────┘
                          │            │
            Частотный     ▼            ▼    Байесовский
          ┌─────────────────────┐  ┌─────────────────────┐
          │ Z-Score & P-Value   │  │ Beta-Binomial MCMC  │
          │ Двусторонний Z-тест │  │ Monte Carlo (10k)   │
          │ Доверит. интервалы  │  │ P(Variant > Control)│
          │ Поправка Манна-Уитни│  │ Expected Loss       │
          └─────────────────────┘  └─────────────────────┘
                          │            │
                          └──────┬─────┘
                                 │
                                 ▼
                   ┌──────────────────────────┐
                   │  Recommendation Engine   │
                   │ Автоматический вердикт   │
                   │   на русском языке       │
                   └──────────────────────────┘
```

---

## 🗄 Схема данных и стриминг ClickHouse

Инициализируется автоматически в [infra/clickhouse/init/init-db.sql](file:///home/kebi/proga/Kebi/synaqcore/infra/clickhouse/init/init-db.sql):

```sql
-- 1. Основная аналитическая таблица
CREATE TABLE IF NOT EXISTS tracking_events (
    timestamp DateTime64(3, 'UTC'),
    user_id String,
    event_id String DEFAULT '',
    event_name String,
    experiment_name String DEFAULT '',
    variant String DEFAULT '',
    event_data String
) ENGINE = ReplacingMergeTree(timestamp)
PARTITION BY toYYYYMM(timestamp)
ORDER BY (event_name, user_id, event_id, toDate(timestamp));

-- 2. Kafka Engine буфер
CREATE TABLE IF NOT EXISTS tracking_events_queue (
    timestamp DateTime64(3, 'UTC'),
    user_id String,
    event_id String DEFAULT '',
    event_name String,
    experiment_name String DEFAULT '',
    variant String DEFAULT '',
    event_data String
) ENGINE = Kafka 
SETTINGS kafka_broker_list = 'redpanda:9092',
         kafka_topic_list = 'tracking_events',
         kafka_group_name = 'clickhouse_tracking_cg',
         kafka_format = 'JSONEachRow',
         input_format_null_as_default=1;

-- 3. Материализованное представление (пайплайн)
CREATE MATERIALIZED VIEW IF NOT EXISTS tracking_events_mv TO tracking_events AS 
SELECT
    timestamp,
    user_id,
    event_id,
    event_name,
    experiment_name,
    variant,
    event_data
FROM tracking_events_queue;
```

---

## 📁 Структура репозитория

```
synaqcore/
├── apps/
│   ├── experiment_api/             # FastAPI: управление экспериментами и выдача вариантов
│   │   ├── src/
│   │   │   ├── api/v1/             # Роуты: admin (эксперименты), client (варианты)
│   │   │   ├── core/               # Конфигурация, Redis, PostgreSQL, JWT-аутентификация
│   │   │   └── services/           # Аллокация, кэш, репозиторий
│   │   └── Dockerfile
│   ├── analytics_api/              # FastAPI: статистика на ClickHouse (bulkhead)
│   ├── dashboard/                  # SPA-дашборд (React + Vite + Tailwind)
│   ├── track_api_go/               # Go микросервис высоконагруженного трекинга
│   │   ├── cmd/api/main.go         # Точка входа сервиса
│   │   ├── internal/kafka/         # Franz-go продюсер для Redpanda
│   │   ├── internal/track/         # HTTP-хендлеры валидации и приёма событий
│   │   └── Dockerfile
│   ├── docker-compose.apps.yml     # Compose спецификация сервисов приложений
│   └── .env.apps.example           # Шаблон конфигурации приложений
├── infra/
│   ├── clickhouse/init/            # DDL инициализации ClickHouse (Kafka Engine + MV)
│   ├── postgres/init/              # Инициализация PostgreSQL
│   ├── zitadel/                    # Bootstrap Zitadel (zitadel-init), public/ и secrets/
│   ├── docker-compose.infra.yml    # Redpanda, ClickHouse, Redis, PostgreSQL, Zitadel
│   └── .env.infra.example          # Шаблон конфигурации инфраструктуры
├── nginx/
│   ├── nginx.conf                  # Конфигурация Reverse Proxy шлюза
│   └── docker-compose.yml          # Compose спецификация шлюза
├── scripts/get_token.sh            # Получение JWT сервисного аккаунта
├── sdk/                            # JS SDK (synaq.js) и демо-симулятор
├── tests/                          # Модульные тесты, схемы и нагрузочные профили
├── docs/                           # Архитектурная документация и спецификации
│   └── SYSTEM_DESIGN.md            # Детальный System Design Document (SDD)
├── docker-compose.yml              # Корневой include-файл для всей системы
└── .gitignore                      # Исключение секретов, артефактов и бинарников
```

---

## 🧪 Тестирование и бенчмарки

### Запуск модульных тестов
```bash
pytest tests/
```

### Нагрузочное тестирование (Locust)
```bash
# Токен нужен, чтобы locust сам создал тестовый эксперимент (без него setup пропускается)
export SYNAQ_TOKEN="$(scripts/get_token.sh)"
locust -f tests/load/locustfile.py --host http://localhost:8080
```

> Gateway ограничивает `/api/v1/variants/` до 50 запросов/с с одного IP (burst 100), сверх лимита — `429`. При нагрузочном тесте с одной машины учитывайте это или временно ослабьте `limit_req` в `nginx/nginx.conf`.

---

## 📄 Лицензия

Проект распространяется под лицензией MIT. Подробности см. в файле `LICENSE`.
