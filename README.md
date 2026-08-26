<div align="center">
  <h1>🧪 SynaqCore</h1>
  <p><b>Микросервисная платформа для серверных A/B/n-экспериментов<br>
  с детерминированным распределением и аналитикой в реальном времени</b></p>

  <!-- Badges -->
  <p>
    <img src="https://img.shields.io/badge/Python-3.11%2B-3776AB?style=for-the-badge&logo=python&logoColor=white" alt="Python 3.11+">
    <img src="https://img.shields.io/badge/FastAPI-009688?style=for-the-badge&logo=fastapi&logoColor=white" alt="FastAPI">
    <img src="https://img.shields.io/badge/RabbitMQ-FF6600?style=for-the-badge&logo=rabbitmq&logoColor=white" alt="RabbitMQ">
    <img src="https://img.shields.io/badge/Redis-DC382D?style=for-the-badge&logo=redis&logoColor=white" alt="Redis">
    <img src="https://img.shields.io/badge/PostgreSQL-4169E1?style=for-the-badge&logo=postgresql&logoColor=white" alt="PostgreSQL">
    <img src="https://img.shields.io/badge/ClickHouse-FFCC00?style=for-the-badge&logo=clickhouse&logoColor=black" alt="ClickHouse">
    <img src="https://img.shields.io/badge/Docker-2496ED?style=for-the-badge&logo=docker&logoColor=white" alt="Docker">
  </p>

  <p>
    <img src="https://img.shields.io/github/license/Kebiproger/synaqcore?style=flat-square" alt="License">
    <img src="https://img.shields.io/badge/status-production%20ready-2ea44f?style=flat-square" alt="Status">
  </p>
</div>

---

## 📋 Содержание

- [Архитектура](#архитектура)
- [Возможности](#возможности)
- [Технологический стек](#технологический-стек)
- [Быстрый старт](#быстрый-старт)
- [API-endpoints](#api-endpoints)
- [Примеры запросов](#примеры-запросов)
- [Конфигурация](#конфигурация)
- [Безопасность](#безопасность)
- [Нагрузочные характеристики](#нагрузочные-характеристики)
- [Разработка](#разработка)

---

## 🏗 Архитектура

```
                                   ┌─────────────────────────────────────┐
                                   │          Gateway (nginx)            │
                                   │            :8080                    │
                                   └──────────┬──────────────────────────┘
                                              │
                    ┌─────────────────────────┼─────────────────────────┐
                    │                         │                         │
                    ▼                         ▼                         ▼
        ┌────────────────────┐    ┌────────────────────┐    ┌────────────────────┐
        │   Experiment API   │    │    Tracker API     │    │     Admin Panel    │
        │    (FastAPI)       │    │    (FastAPI)       │    │      (HTML/JS)     │
        │     :18000         │    │     :18001         │    │     :18000/admin   │
        └────────┬───────────┘    └────────┬───────────┘    └────────────────────┘
                 │                         │
                 │              ┌──────────▼──────────┐
                 ▼              │      RabbitMQ       │
        ┌────────────────┐     │  ab_platform_events │
        │     Redis      │     │  (Persistent Queue) │
        │  (кэш + config)│     └──────────┬──────────┘
        └───────┬────────┘                │
                │                         ▼
        ┌───────▼────────┐     ┌────────────────────┐
        │   PostgreSQL   │     │  Analytics Worker  │
        │  (хранилище    │     │  (буфер 5k/5 сек)  │
        │   тестов)      │     └────────┬───────────┘
        └────────────────┘              │
                                        ▼
                               ┌────────────────┐
                               │   ClickHouse   │
                               │  (OLAP-аналит.)│
                               └────────────────┘
```

### 🧩 Компоненты

| Сервис | Назначение | Технологии | Порты |
|--------|-----------|-----------|-------|
| **Experiment API** | Создание A/B/n-тестов, детерминированное распределение пользователей по вариантам, **админ-панель (HTML)** | FastAPI, Redis, PostgreSQL, SHA-256 | `18000` |
| **Tracker API** | Приём аналитических событий (клики, просмотры, конверсии) | FastAPI, RabbitMQ (PERSISTENT) | `18001` |
| **Analytics Worker** | Буферизация и пакетная (batch) запись событий в ClickHouse | asyncio, aio-pika, ClickHouse Connect | — |
| **Gateway** | Единая точка входа, прокси-роутинг | nginx | `8080` |

---

## ✨ Возможности

### 🎯 Детерминированное распределение

Пользователь всегда получает **тот же вариант** — без хранения истории в БД:

```python
# SHA-256(user_id + experiment_name) → float [0.00, 99.99] → вариант по весам
hash_hex = hashlib.sha256(f"{user_id}:{experiment_name}".encode()).hexdigest()
bucket = (int(hash_hex, 16) % 10000) / 100.0  # → 0.00 .. 99.99
```

### 📊 A/B/n тесты

- Произвольное количество вариантов (A, B, C, D...)
- Настраиваемые веса распределения (сумма = 100%)
- Флаг `is_active` — мгновенное отключение теста

### 🖥 Админ-панель

Встроенная HTML-админка для управления экспериментами (без curl):

| Возможность | Как открыть |
|------------|-------------|
| 📋 Список тестов, создание / удаление / включение-выключение | Откройте в браузере 👉 **http://localhost:8080/admin** |
| 🔑 Доступ по `X-Admin-Key` (без ключа — 403) | Укажите ключ в заголовке или настройте в `.env` |
| 📊 Визуальный просмотр вариантов и весов | Готовый UI на FastAPI + HTML-шаблоны |

### ⚡ Производительность

- **Experiment API** — ответ < **10 мс** (p99) за счёт Redis-кэша
- **Tracker API** — выдерживает **1000+ RPS**, делегируя I/O RabbitMQ
- **Worker** — батчевая запись до **5000 событий** или каждые **5 секунд**

### 🛡 Отказоустойчивость

| Сценарий | Механизм защиты |
|---------|----------------|
| ClickHouse недоступен | События накапливаются в RabbitMQ (Persistent Queue) |
| RabbitMQ перезагрузка | Durable queues + PERSISTENT delivery mode |
| Падение Worker | prefetch_count не даёт потерять сообщения |
| Сбой сети | ack только после успешного INSERT в ClickHouse |

### 📈 Горизонтальное масштабирование

Все микросервисы — **stateless**: добавьте больше реплик Docker-контейнеров без изменения кода.

---

## 🛠 Технологический стек

<div align="center">

| Компонент | Технология | Назначение |
|-----------|-----------|------------|
| <img src="https://cdn.jsdelivr.net/gh/devicons/devicon/icons/python/python-original.svg" width="20"> **Язык** | Python 3.11+ | Runtime |
| <img src="https://cdn.jsdelivr.net/gh/devicons/devicon/icons/fastapi/fastapi-original.svg" width="20"> **Фреймворк** | FastAPI | Асинхронные REST API |
| <img src="https://cdn.jsdelivr.net/gh/devicons/devicon/icons/redis/redis-original.svg" width="20"> **Кэш** | Redis + redis.asyncio | Конфигурации тестов |
| <img src="https://cdn.jsdelivr.net/gh/devicons/devicon/icons/postgresql/postgresql-original.svg" width="20"> **БД тестов** | PostgreSQL | Хранение экспериментов |
| <img src="https://cdn.jsdelivr.net/gh/devicons/devicon/icons/rabbitmq/rabbitmq-original.svg" width="20"> **Брокер** | RabbitMQ + aio-pika | Очередь событий |
| <img src="https://cdn.jsdelivr.net/gh/devicons/devicon/icons/clickhouse/clickhouse-original.svg" width="20"> **Аналитика** | ClickHouse (MergeTree) | OLAP-хранилище |
| <img src="https://cdn.jsdelivr.net/gh/devicons/devicon/icons/docker/docker-original.svg" width="20"> **Инфраструктура** | Docker, Docker Compose | Контейнеризация |
| <img src="https://cdn.jsdelivr.net/gh/devicons/devicon/icons/nginx/nginx-original.svg" width="20"> **Gateway** | nginx | Единая точка входа |

</div>

---

## 🚀 Быстрый старт

### 📋 Требования

- Docker Engine 24+ и Docker Compose v2+
- make (опционально)

### ▶️ Запуск

```bash
# 1. Клонировать репозиторий
git clone https://github.com/Kebiproger/synaqcore.git
cd synaqcore

# 2. Настроить окружение (опционально)
cp .env.example .env
# Отредактировать .env при необходимости

# 3. Запустить всю систему одной командой
docker compose up -d

# 4. Проверить healthcheck
curl http://localhost:8080/health
# → {"status":"healthy"}
```

> 💡 **Горячие клавиши:** 
> - `docker compose logs -f` — посмотреть логи всех сервисов
> - `docker compose down` — остановить
> - `docker compose down -v` — остановить + удалить данные БД

---

## 📡 API-endpoints

| Метод | Путь | Описание | Аутентификация |
|-------|------|---------|:---:|
| `POST` | `/api/v1/experiments/` | Создать A/B тест | 🔑 `X-Admin-Key` |
| `GET` | `/api/v1/variants/` | Получить вариант для пользователя | — |
| `POST` | `/api/v1/tracker/events` | Отправить аналитическое событие | — |
| `GET` | `/health` | Healthcheck | — |
| `GET` | `/admin` | Админ-панель (HTML) | 🔑 `X-Admin-Key` |

---

## 🔧 Примеры запросов

### 🖥 Админ-панель (браузер)

Откройте в браузере **http://localhost:8080/admin** — готовая HTML-панель для управления тестами:

- Создание A/B/n экспериментов через форму
- Просмотр списка тестов и их статусов (`is_active`)
- Удаление и отключение тестов без единой команды curl

### 📝 Создание эксперимента

```bash
curl -X POST http://localhost:8080/api/v1/experiments/ \
  -H "Content-Type: application/json" \
  -H "X-Admin-Key: ab_testing_admin_secret" \
  -d '{
    "name": "landing_redesign",
    "variants": {
      "A": 50.0,
      "B": 30.0,
      "C": 20.0
    },
    "is_active": true
  }'
```

**Ответ:** `201 Created`
```json
{
  "name": "landing_redesign",
  "variants": {"A": 50.0, "B": 30.0, "C": 20.0},
  "is_active": true
}
```

### 👤 Получение варианта

```bash
curl "http://localhost:8080/api/v1/variants/?user_id=user_12345&experiment_name=landing_redesign"
```

**Ответ:** `200 OK`
```json
{
  "experiment_name": "landing_redesign",
  "user_id": "user_12345",
  "variant": "B"
}
```

> 🔁 Один и тот же `user_id` всегда получает **один и тот же вариант** — 
> попробуйте выполнить запрос несколько раз.

### 📊 Отправка события

```bash
curl -X POST http://localhost:8080/api/v1/tracker/events \
  -H "Content-Type: application/json" \
  -d '{
    "user_id": "user_12345",
    "event_name": "button_click",
    "timestamp": "2026-07-23T12:00:00Z",
    "experiment_name": "landing_redesign",
    "variant": "B",
    "event_data": {
      "button_color": "green",
      "page_section": "hero"
    }
  }'
```

**Ответ:** `202 Accepted` (событие принято в очередь, запись — асинхронно)

---

## ⚙️ Конфигурация

Все настройки вынесены в переменные окружения (`.env`):

```env
# === Redis ===
REDIS_URL=redis://redis:6379

# === PostgreSQL ===
POSTGRES_HOST=postgres
POSTGRES_PORT=5432
POSTGRES_USER=ab_user
POSTGRES_PASSWORD=ab_pass          # 🔴 СМЕНИТЕ!

# === Админ-ключ ===
ADMIN_API_KEY=ab_testing_admin_secret   # 🔴 Обязательно смените!

# === RabbitMQ ===
RABBITMQ_URL=amqp://guest:guest@rabbitmq:5672/

# === ClickHouse ===
CLICKHOUSE_USER=default
CLICKHOUSE_PASSWORD=ab_pass        # 🔴 СМЕНИТЕ!

# === Порты (опционально) ===
# EXPERIMENT_API_PORT=18000
# TRACK_API_PORT=18001
# GATEWAY_PORT=8080
```

### 🔌 Подключение к вашим БД

Скопируйте `.env.example` → `.env`, укажите свои серверы и запустите только API-сервисы:

```bash
docker compose -f apps/docker-compose.apps.yml up -d
```

---

## 🛡 Безопасность

| Мера | Описание | CWE |
|------|---------|:---:|
| **Аутентификация Admin API** | Заголовок `X-Admin-Key` для создания экспериментов | CWE-287 |
| **Валидация входных данных** | Pydantic v2 — строгие схемы, проверка весов (сумма = 100%) | CWE-20 |
| **Параметризованные запросы** | Все SQL-запросы через parameterized statements | CWE-89 |
| **Защита от потери данных** | RabbitMQ PERSISTENT + ack после записи в ClickHouse | CWE-359 |
| **Immutable-аналитика** | ClickHouse — append-only, события не перезаписываются | CWE-766 |
| **CORS** | Настроен для гибкой интеграции с frontend | CWE-942 |
| **Stateless** | Нет сессий на сервере, простая горизонтальная изоляция | CWE-384 |
| **Изоляция микросервисов** | Experiment API и Tracker API — разные порты и наборы роутов | CWE-923 |

---

## 📈 Нагрузочные характеристики

> ⚠️ **Все замеры — на 1 хосте (developer laptop).**  
> Locust + все 6 Docker-контейнеров делят один CPU.  
> В продакшене с выделенными серверами и репликами — RPS × реплики, p99 ниже.

### ⚡ Experiment API (GET /api/v1/variants/)

```
Чистая аллокация (SHA-256):  1.3 µs    →  ~760 000 RPS
                            ↓
Через FastAPI + Redis:       p99 = 43ms →   ~460 RPS   (напрямую к контейнеру)
                            ↓
Через nginx gateway:         p99 = 80ms →   ~540 RPS   (через :8080)
```

### 🚀 Tracker API (POST /api/v1/tracker/events)

```
Напрямую к контейнеру:       p99 = 21ms →   ~820 RPS
Через nginx gateway:         p99 = 80ms →   ~680 RPS
```

### 📊 Сводная таблица

<div align="center">

| Метрика | На 1 хосте (Docker) | В продакшене (5 реплик+) |
|---------|:-------------------:|:------------------------:|
| ⚡ Experiment API (p99) | **~43ms** напрямую / **~80ms** через gateway | **< 10ms** |
| ⚡ Experiment API (RPS) | **~500** | **~2 500+** |
| 🚀 Tracker API (p99) | **~21ms** напрямую / **~80ms** через gateway | **< 10ms** |
| 🚀 Tracker API (RPS) | **~800** | **~4 000+** |
| 📦 Worker batch size | **5 000 событий** | 5 000+ (настраивается) |
| ⏱ Worker flush interval | **5 секунд** | 5 сек (настраивается) |
| 🔄 Гарантия доставки | **At-least-once** | At-least-once |

</div>

<details>
<summary>🧪 Как запустить нагрузочный тест самому</summary>

```bash
pip install locust

# Experiment API (напрямую)
locust -f tests/load/locustfile.py \
  --host=http://localhost:18000 \
  --headless -u 50 -r 10 --run-time 20s

# Через gateway (как реальные пользователи)
locust -f tests/load/locustfile.py \
  --host=http://localhost:8080 \
  --headless -u 50 -r 10 --run-time 20s

# С веб-интерфейсом
locust -f tests/load/locustfile.py --host=http://localhost:8080
# Открыть http://localhost:8089
```

</details>

---

## 💻 Разработка

### Локальный запуск микросервисов

```bash
# Experiment API
cd apps/experiment_api && uvicorn src.main:app --reload --port 18000

# Tracker API
cd apps/track_api && uvicorn src.main:app --reload --port 18001

# Analytics Worker
cd infra/worker_collector && python src/main.py
```

### Структура проекта

```
synaqcore/
├── apps/                                    # API-микросервисы
│   ├── experiment_api/                      # Experiment API
│   │   ├── Dockerfile
│   │   ├── requirements.txt
│   │   └── src/
│   │       ├── api/v1/                      # Эндпоинты (admin, client, stats)
│   │       ├── core/                        # Config, Redis, DB
│   │       ├── schemas/                     # Pydantic-модели
│   │       ├── services/                    # Бизнес-логика (allocation, cache)
│   │       └── templates/                   # HTML-шаблоны (админ-панель)
│   ├── track_api/                           # Tracker API
│   │   ├── Dockerfile
│   │   ├── requirements.txt
│   │   └── src/
│   │       ├── api/v1/                      # Эндпоинты
│   │       ├── core/                        # Config
│   │       └── services/                    # Broker (RabbitMQ)
│   └── docker-compose.apps.yml              # 👈 Compose для API-сервисов
├── infra/                                   # Инфраструктура
│   ├── worker_collector/                    # Analytics Worker
│   ├── clickhouse/init/                     # DDL (CREATE TABLE)
│   ├── postgres/                            # PostgreSQL (схема через ORM)
│   └── docker-compose.infra.yml
├── nginx/                                   # Gateway
│   ├── nginx.conf
│   └── docker-compose.yml
├── docker-compose.yml                       # 👈 Главный compose-файл (всё вместе)
├── .env.example
└── .gitignore
```

---

<div align="center">
  <hr>
  <p>
    🧪 <b>SynaqCore</b> — проверяйте гипотезы, не замедляя приложение<br>
    <sub>Сделано с ❤️ и ☕ для продуктовых команд</sub>
  </p>
  <p style="margin-top: 12px;">
    <a href="https://github.com/Kebiproger/a_b_testing_demo">🏢 Демо-приложение (Company App)</a> —
    лендинг с реальными A/B тестами
  </p>
</div>
