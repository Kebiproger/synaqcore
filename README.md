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
- [API-интерфейсы](#-api-интерфейсы)
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
         /api/v1/experiments, /admin, /stats                         /api/v1/tracker/events
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
* Пользователь с одним и тем же `user_id` всегда попадает в один и тот же вариант эксперимента без необходимости сохранять историю распределения в базе данных:
  $$\text{bucket} = \left(\text{SHA-256}(\text{user\_id} : \text{experiment\_name}) \pmod{10000}\right) / 100.0$$
* Горячий кэш конфигов и весов в Redis обеспечивает ответ на выдачу варианта за $< 2\text{ ms}$.

### 4. 🧠 Двухуровневая аналитика (Smart + Raw)
* **Smart Analytics (`/api/v1/stats/{exp}`):**
  * Частотный анализ: $Z$-test, p-value, 95% доверительные интервалы, относительный uplift.
  * Байесовский анализ: симуляция Монте-Карло, вероятность $P(B > A)$, Expected Loss.
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
| **Admin Panel** | HTML5, Vanilla JS, CSS3 | Встроенный дашборд управления тестами и мониторинга аналитики | `/admin` |
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
Скопируйте шаблоны конфигураций (значения по умолчанию настроены для локального окружения):
```bash
cp apps/.env.apps.example apps/.env.apps
cp infra/.env.infra.example infra/.env.infra
```

### 3. Запуск всех сервисов платформы
Запуск всей инфраструктуры и приложений выполняется единой командой:
```bash
docker compose up -d --build
```

Все внутренние порты СУБД изолированы во внутренней docker-сети `ab_testing_net`. Наружу смотрит только Gateway на порту `8080`.

### 4. Доступ к интерфейсам
* 🌐 **Панель администратора:** [http://localhost:8080/admin](http://localhost:8080/admin)
* 📖 **Swagger OpenAPI документация:** [http://localhost:8080/docs](http://localhost:8080/docs) (через Experiment API)
* 🧪 **Клиентское демо-приложение:** [http://localhost:5000](http://localhost:5000) (если запущен `company_app`)

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

### 2. Список активных экспериментов
```bash
curl -X GET "http://localhost:8080/api/v1/variants/active"
```

### 3. Отправка аналитического события (Go Tracker)
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

### 4. Умная аналитика A/B теста
```bash
curl -X GET "http://localhost:8080/api/v1/stats/landing_button_test?goal_event=cta_click&method=both"
```

### 5. Базовая сводка по эксперименту
```bash
curl -X GET "http://localhost:8080/api/v1/stats/landing_button_test/basic"
```

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
    event_name String,
    experiment_name Nullable(String),
    variant Nullable(String),
    event_data String
) ENGINE = MergeTree()
ORDER BY (event_name, toDate(timestamp), user_id);

-- 2. Kafka Engine буфер
CREATE TABLE IF NOT EXISTS tracking_events_queue (
    timestamp DateTime64(3, 'UTC'),
    user_id String,
    event_name String,
    experiment_name Nullable(String),
    variant Nullable(String),
    event_data String
) ENGINE = Kafka 
SETTINGS kafka_broker_list = 'redpanda:9092',
         kafka_topic_list = 'tracking_events',
         kafka_group_name = 'clickhouse_tracking_cg',
         kafka_format = 'JSONEachRow';

-- 3. Материализованное представление (пайплайн)
CREATE MATERIALIZED VIEW IF NOT EXISTS tracking_events_mv TO tracking_events AS 
SELECT
    timestamp,
    user_id,
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
│   ├── experiment_api/             # Python FastAPI микросервис
│   │   ├── src/
│   │   │   ├── api/v1/             # Роуты: admin, client, stats
│   │   │   ├── core/               # Конфигурация, Redis, PostgreSQL
│   │   │   ├── services/           # ClickHouse Aggregator, статистика, NLP-рекомендации
│   │   │   └── templates/          # HTML5 Админ-панель
│   │   └── Dockerfile
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
│   ├── docker-compose.infra.yml    # Redpanda, ClickHouse, Redis, PostgreSQL
│   └── .env.infra.example          # Шаблон конфигурации инфраструктуры
├── nginx/
│   ├── nginx.conf                  # Конфигурация Reverse Proxy шлюза
│   └── docker-compose.yml          # Compose спецификация шлюза
├── tests/                          # Интеграционные тесты, схемы и нагрузочные профили
├── docs/                           # Архитектурная документация и спецификации
│   ├── SYSTEM_DESIGN.md            # Детальный System Design Document (SDD)
│   └── REFACTORING.md              # Спецификация миграции и оптимизации
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
locust -f tests/load/locustfile.py --host http://localhost:8080
```

---

## 📄 Лицензия

Проект распространяется под лицензией MIT. Подробности см. в файле `LICENSE`.
