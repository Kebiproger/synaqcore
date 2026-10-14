# 🔌 SynaqCore — Руководство по интеграции в инфраструктуру компании

Данное руководство предназначено для **DevOps-инженеров, Frontend- и Backend-разработчиков**, внедряющих платформу A/B-тестирования SynaqCore в существующий технологический стек компании.

---

## 📑 Содержание

1. [Архитектура интеграции](#1-архитектура-интеграции)
2. [DevOps & Сетевой слой (Инфраструктура)](#2-devops--сетевой-слой-инфраструктура)
   - [Модель изоляции Zero-Trust](#модель-изоляции-zero-trust)
   - [Вариант А: Выделенный поддомен (ab.company.com)](#вариант-а-выделенный-поддомен-abcompanycom)
   - [Вариант Б: Путь на основном домене (company.com/synaq/)](#вариант-б-путь-на-основном-домене-companycomsynaq)
   - [Ограничение доступа к панели управления (/dashboard/)](#ограничение-доступа-к-панели-управления-dashboard)
3. [Клиентская интеграция (Frontend / Mobile)](#3-клиентская-интеграция-frontend--mobile)
   - [Установка @synaq/client (NPM)](#установка-synaqclient-npm)
   - [Подключение через CDN (без сборщика)](#подключение-через-cdn-без-сборщика)
   - [Инициализация клиента](#инициализация-клиента)
   - [Получение варианта (getVariant)](#получение-варианта-getvariant)
   - [Пакетное получение вариантов (getVariantsBatch)](#пакетное-получение-вариантов-getvariantsbatch)
   - [Отправка событий конверсий (track & sendBeacon)](#отправка-событий-конверсий-track--sendbeacon)
   - [Интеграция в React / Next.js](#интеграция-в-react--nextjs)
4. [Серверная интеграция (Backend-to-Backend)](#4-серверная-интеграция-backend-to-backend)
   - [Получение варианта по REST API](#получение-варианта-по-rest-api)
   - [Отправка серверных событий конверсий](#отправка-серверных-событий-конверсий)
   - [Пример на Python (requests / httpx)](#пример-на-python-requests--httpx)
   - [Пример на Go (net/http)](#пример-на-go-nethttp)
   - [Пример на Node.js](#пример-на-nodejs)
5. [M2M автоматизация и CI/CD (Admin API)](#5-m2m-автоматизация-и-cicd-admin-api)
   - [Получение JWT токена сервисного аккаунта](#получение-jwt-токена-сервисного-аккаунта)
   - [Управление экспериментами через API](#управление-экспериментами-через-api)
   - [Выгрузка статистики в корпоративные BI](#выгрузка-статистики-в-корпоративные-bi)
6. [Best Practices & Безопасность](#6-best-practices--безопасность)

---

## 1. Архитектура интеграции

Платформа SynaqCore спроектирована по принципу **минимального воздействия на пользовательский опыт**:

```
[Пользователь / Браузер] ────────────► [Cloudflare Edge / Корпоративный WAF]
                                                        │
                                                        ▼
                                          [Корпоративный Nginx (DMZ)]
                                                        │
                                                        ▼ (127.0.0.1:8080)
                                            [SynaqCore Gateway (Nginx)]
                                                        │
                      ┌─────────────────────────────────┴─────────────────────────────────┐
                      │                                                                   │
           [Клиентские запросы]                                                [Телеметрия событий]
                      │                                                                   │
                      ▼                                                                   ▼
       [FastAPI: Experiment API]                                              [Go: Track API Core]
        - Выдача варианта (<2ms)                                               - Пропускная способность:
        - Кэш в Redis                                                            20k+ RPS в Redpanda
```

* **Чтение (Выдача варианта):** Публичный эндпоинт `/api/v1/variants/{exp}`. Кэшируется в Redis, среднее время ответа `< 2 мс`. Детерминированный хэш гарантирует, что пользователь всегда видит свой вариант.
* **Запись (Трекинг событий):** Высокопроизводительный микросервис на Go `/api/v1/tracker/events`. Асинхронно пушит события в Kafka/Redpanda без промежуточных тяжелых БД.
* **Изоляция:** Аналитика считается на стороне ClickHouse и не влияет на скорость выдачи вариантов.

---

## 2. DevOps & Сетевой слой (Инфраструктура)

### Модель изоляции Zero-Trust

Все внутренние базы данных и брокеры сообщений (**PostgreSQL, Redis, ClickHouse, Redpanda**) развернуты во внутренней Docker-сети `ab_testing_net` и **не имеют открытых наружу портов** на хост-машине.

Наружу выставлены только два порта, жестко привязанные к `127.0.0.1`:
* `127.0.0.1:8080` — **SynaqCore Gateway** (раздача SDK, API вариантов, сбор трекинга, дашборд).
* `127.0.0.1:8081` — **Zitadel IAM** (SSO-аутентификация для сотрудников и сервисных аккаунтов).

Готовый эталонный конфигурационный файл для вашего Nginx:  
👉 [nginx/company-edge-integration.example.conf](file:///home/kebi/proga/Kebi/synaqcore/nginx/company-edge-integration.example.conf)

---

### Вариант А: Выделенный поддомен (`ab.company.com`)
*(Рекомендуемый вариант для простоты настройки CORS, SSL и маршрутизации)*

В конфигурации внешнего Nginx компании:

```nginx
upstream synaqcore_gateway {
    server 127.0.0.1:8080;
    keepalive 64;
}

server {
    listen 443 ssl http2;
    server_name ab.company.com;

    ssl_certificate /etc/ssl/certs/company.crt;
    ssl_certificate_key /etc/ssl/private/company.key;

    # Проброс реального IP пользователя из Cloudflare или внешнего соединения
    proxy_set_header Host $http_host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header CF-Connecting-IP $http_cf_connecting_ip;

    # 1. Раздача JS SDK клиентам (кэшируется на Nginx)
    location /sdk/ {
        proxy_pass http://synaqcore_gateway;
        proxy_cache_valid 200 1h;
    }

    # 2. Публичные запросы получения вариантов
    location /api/v1/variants/ {
        proxy_pass http://synaqcore_gateway;
        proxy_read_timeout 3s;
    }

    # 3. Публичный приём аналитических кликов
    location /api/v1/tracker/ {
        proxy_pass http://synaqcore_gateway;
        proxy_read_timeout 3s;
    }

    # 4. Панель управления (закрыта для внешнего мира)
    location /dashboard/ {
        allow 192.168.1.0/24; # Офисная подсеть или VPN
        deny all;
        proxy_pass http://synaqcore_gateway;
    }
}
```

---

### Вариант Б: Путь на основном домене (`company.com/synaq/`)

Если в компании принято всё размещать на одном домене:

```nginx
location /synaq/ {
    # Срезаем префикс /synaq/ при передаче во внутренний шлюз
    rewrite ^/synaq/(.*) /$1 break;
    proxy_pass http://127.0.0.1:8080;
    
    proxy_set_header Host $http_host;
    proxy_set_header X-Forwarded-Prefix /synaq;
    proxy_set_header CF-Connecting-IP $http_cf_connecting_ip;
}
```

---

### Ограничение доступа к панели управления (`/dashboard/`)

Панель управления экспериментами предназначена только для продуктовых менеджеров, аналитиков и инженеров компании. Доступ клиентов к ней должен быть закрыт одним из способов:

1. **Белый список IP (VPN / Офис):**
   ```nginx
   location /dashboard/ {
       allow 10.0.0.0/8;        # Внутренний корпоративный VPN
       allow 198.51.100.24;     # Статический IP офиса
       deny all;
       proxy_pass http://synaqcore_gateway;
   }
   ```
2. **Cloudflare Access (Zero-Trust SSO):**
   ```nginx
   location /dashboard/ {
       if ($http_cf_access_authenticated_user_email = "") {
           return 403 "Forbidden: Cloudflare Access login required";
       }
       proxy_pass http://synaqcore_gateway;
   }
   ```

---

## 3. Клиентская интеграция (Frontend / Mobile)

### Установка `@synaq/client` (NPM)

Для SPA/SSR проектов (React, Vue, Next.js, Svelte, Angular):

```bash
npm install @synaq/client
# или
pnpm add @synaq/client
```

Файлы пакета:
* [sdk/package.json](file:///home/kebi/proga/Kebi/synaqcore/sdk/package.json)
* [sdk/synaq.d.ts](file:///home/kebi/proga/Kebi/synaqcore/sdk/synaq.d.ts) — полная типизация TypeScript.
* [sdk/synaq.js](file:///home/kebi/proga/Kebi/synaqcore/sdk/synaq.js) — ядро библиотеки (zero-dependency, < 3 КБ).

---

### Подключение через CDN (без сборщика)

Для простых HTML-лендингов, Tilda, WordPress или legacy-проектов:

```html
<!-- Подключение SDK со шлюза SynaqCore -->
<script src="https://ab.company.com/sdk/synaq.js"></script>

<script>
  const synaq = new SynaqClient({
    baseUrl: 'https://ab.company.com'
  });
</script>
```

---

### Инициализация клиента

```typescript
import SynaqClient from '@synaq/client';

export const synaq = new SynaqClient({
  baseUrl: 'https://ab.company.com', // URL шлюза SynaqCore
  timeoutMs: 1500,                   // Предельное время ожидания ответа (по умолч. 2000 мс)
  fallbackVariant: 'control',        // Вариант по умолчанию при сетевой ошибке
  enableCache: true                  // Сохранять варианты в localStorage для мгновенного рендера
});
```

> **Важно:** Идентификатор пользователя `user_id` генерируется автоматически (UUIDv4) и сохраняется в `localStorage`. Если пользователь авторизовался, можно передать его внутренний ID:  
> `const synaq = new SynaqClient({ baseUrl: '...', userId: user.id });`

---

### Получение варианта (`getVariant`)

```typescript
async function initHeroBanner() {
  // Запрашиваем вариант для эксперимента 'new_homepage_hero'
  const variant = await synaq.getVariant('new_homepage_hero');

  const banner = document.getElementById('hero-banner');
  if (variant === 'variant_b') {
    banner.innerHTML = '<h1>Скидка 30% на все заказы только сегодня!</h1>';
  } else {
    banner.innerHTML = '<h1>Добро пожаловать в наш онлайн-магазин!</h1>';
  }
}

initHeroBanner();
```

---

### Пакетное получение вариантов (`getVariantsBatch`)

Если страница содержит сразу несколько тестов, чтобы не делать серию отдельных запросов, используйте пакетную выборку:

```typescript
const variants = await synaq.getVariantsBatch([
  'hero_banner_test',
  'search_algorithm_v2',
  'cta_button_color'
]);

console.log(variants);
// Результат:
// {
//   hero_banner_test: 'variant_b',
//   search_algorithm_v2: 'control',
//   cta_button_color: 'green'
// }
```

---

### Отправка событий конверсий (`track` & `sendBeacon`)

События отправляются асинхронно и никогда не блокируют интерфейс:

```typescript
const buyBtn = document.getElementById('checkout-button');

buyBtn.addEventListener('click', () => {
  synaq.track('cta_click', {
    experimentName: 'cta_button_color',
    data: { 
      cart_total: 15000, 
      items_count: 3 
    },
    useBeacon: true // Использует navigator.sendBeacon при уходе со страницы
  });
});
```

---

### Интеграция в React / Next.js

Пример хука `useVariant` для чистого рендеринга без мигания интерфейса:

```tsx
import React, { useState, useEffect } from 'react';
import { synaq } from './synaq-service';

export function useVariant(experimentName: string, defaultVariant = 'control') {
  const [variant, setVariant] = useState<string>(defaultVariant);
  const [isLoading, setIsLoading] = useState<boolean>(true);

  useEffect(() => {
    let isMounted = true;
    synaq.getVariant(experimentName)
      .then((assigned) => {
        if (isMounted) {
          setVariant(assigned);
          setIsLoading(false);
        }
      })
      .catch(() => {
        if (isMounted) setIsLoading(false);
      });

    return () => { isMounted = false; };
  }, [experimentName]);

  return { variant, isLoading };
}

// Пример использования в компоненте:
export function SubscribeButton() {
  const { variant } = useVariant('subscription_cta');

  const handleClick = () => {
    synaq.track('subscribe_click', { experimentName: 'subscription_cta' });
  };

  if (variant === 'accent_orange') {
    return <button onClick={handleClick} className="btn-orange">Оформить премиум</button>;
  }

  return <button onClick={handleClick} className="btn-default">Подписаться</button>;
}
```

---

## 4. Серверная интеграция (Backend-to-Backend)

Для критически важных операций (оплата, списание бонусов, финансовые транзакции, серверный рендеринг SSR) трекинг должен производиться на бэкенде компании.

### Получение варианта по REST API

* **Метод:** `GET /api/v1/variants/{experiment_name}?user_id={user_id}`
* **Аутентификация:** Не требуется (публичный быстрый эндпоинт).

**Ответ (200 OK):**
```json
{
  "experiment": "pricing_model_v2",
  "variant": "subscription_annual",
  "user_id": "usr_99812",
  "config": {
    "price_per_month": 1200,
    "discount": 20
  }
}
```

---

### Отправка серверных событий конверсий

* **Метод:** `POST /api/v1/tracker/events`
* **Content-Type:** `application/json`
* **Аутентификация:** Не требуется.

**Тело запроса:**
```json
{
  "user_id": "usr_99812",
  "event_name": "payment_success",
  "experiment_name": "pricing_model_v2",
  "variant": "subscription_annual",
  "event_data": "{\"amount\":14400,\"currency\":\"RUB\",\"order_id\":\"ord_123\"}"
}
```

---

### Пример на Python (requests / httpx)

```python
import httpx
import json

SYNAQ_BASE_URL = "http://127.0.0.1:8080" # или https://ab.company.com

client = httpx.Client(base_url=SYNAQ_BASE_URL, timeout=1.0)

def get_user_variant(experiment_name: str, user_id: str) -> str:
    """Получение варианта с автоматическим fail-safe фолбэком."""
    try:
        resp = client.get(f"/api/v1/variants/{experiment_name}", params={"user_id": user_id})
        if resp.status_code == 200:
            return resp.json().get("variant", "control")
    except Exception as e:
        # При сбое сети сервис продолжает бесперебойную работу на дефолтной логике
        pass
    return "control"

def track_purchase_event(user_id: str, experiment_name: str, variant: str, order_id: str, amount: float):
    """Отправка события успешной оплаты в ClickHouse телеметрию."""
    payload = {
        "user_id": user_id,
        "event_name": "payment_success",
        "experiment_name": experiment_name,
        "variant": variant,
        "event_data": json.dumps({"order_id": order_id, "amount": amount})
    }
    try:
        client.post("/api/v1/tracker/events", json=payload)
    except Exception:
        pass
```

---

### Пример на Go (`net/http`)

```go
package synaq

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type SynaqClient struct {
	baseURL    string
	httpClient *http.Client
}

func NewSynaqClient(baseURL string) *SynaqClient {
	return &SynaqClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 800 * time.Millisecond,
		},
	}
}

func (s *SynaqClient) GetVariant(experimentName, userID string) string {
	url := fmt.Sprintf("%s/api/v1/variants/%s?user_id=%s", s.baseURL, experimentName, userID)
	resp, err := s.httpClient.Get(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		return "control" // Fail-safe fallback
	}
	defer resp.Body.Close()

	var data struct {
		Variant string `json:"variant"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "control"
	}
	return data.Variant
}

func (s *SynaqClient) Track(eventName, expName, variant, userID string, payload map[string]interface{}) {
	rawPayload, _ := json.Marshal(payload)
	body, _ := json.Marshal(map[string]interface{}{
		"user_id":         userID,
		"event_name":      eventName,
		"experiment_name": expName,
		"variant":         variant,
		"event_data":      string(rawPayload),
	})

	go func() {
		// Асинхронная неблокирующая отправка
		s.httpClient.Post(s.baseURL+"/api/v1/tracker/events", "application/json", bytes.NewBuffer(body))
	}()
}
```

---

### Пример на Node.js

```typescript
const SYNAQ_URL = process.env.SYNAQ_URL || 'http://127.0.0.1:8080';

export async function getVariant(experiment: string, userId: string): Promise<string> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 1000);

  try {
    const res = await fetch(`${SYNAQ_URL}/api/v1/variants/${experiment}?user_id=${userId}`, {
      signal: controller.signal
    });
    if (res.ok) {
      const data = await res.json();
      return data.variant || 'control';
    }
  } catch (err) {
    // Тихо возвращаем дефолтный вариант при таймауте
  } finally {
    clearTimeout(timer);
  }
  return 'control';
}
```

---

## 5. M2M автоматизация и CI/CD (Admin API)

### Получение JWT токена сервисного аккаунта

Для интеграции с CI/CD (GitLab CI, GitHub Actions) или корпоративным бэкендом используется сервисный аккаунт Zitadel `synaq-automation` (OAuth 2.0 `client_credentials`):

Готовый bash-скрипт в репозитории:  
👉 [scripts/get_token.sh](file:///home/kebi/proga/Kebi/synaqcore/scripts/get_token.sh)

```bash
# Получение токена:
export SYNAQ_TOKEN="$(scripts/get_token.sh)"

# Проверка:
curl -H "Authorization: Bearer $SYNAQ_TOKEN" http://localhost:8080/api/v1/experiments/
```

---

### Управление экспериментами через API

**Создание эксперимента (роль `admin`):**
```bash
curl -X POST "http://localhost:8080/api/v1/experiments/" \
  -H "Authorization: Bearer $SYNAQ_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "checkout_new_flow",
    "description": "Тестирование одностраничного чекаута",
    "variants": {
      "control": 50,
      "one_page": 50
    }
  }'
```

**Остановка эксперимента (роль `admin`):**
```bash
curl -X PATCH "http://localhost:8080/api/v1/experiments/checkout_new_flow" \
  -H "Authorization: Bearer $SYNAQ_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"is_active": false}'
```

---

### Выгрузка статистики в корпоративные BI

Аналитический микросервис возвращает готовые математические расчеты (частотные Z-тесты, p-value, 95% доверительные интервалы и байесовские оценки Monte-Carlo):

```bash
curl -X GET "http://localhost:8080/api/v1/stats/checkout_new_flow?goal_event=payment_success&method=both" \
  -H "Authorization: Bearer $SYNAQ_TOKEN"
```

---

## 6. Best Practices & Безопасность

1. **Правило Fallback Variant:**  
   Всегда проектируйте логику приложений так, чтобы при возврате `control` интерфейс работал штатно. Сетевые таймауты SDK настроены на безопасные 1–2 секунды, предотвращая зависание рендера.
2. **Защита от фрода и накруток (Дедупликация):**  
   ClickHouse использует таблицу `ReplacingMergeTree` с агрегацией `countDistinct(user_id)`. Если один и тот же бот или пользователь кликнет по кнопке 1000 раз, в расчет конверсии пойдет ровно **1 уникальный клик**.
3. **Безопасность персональных данных (Zero PII):**  
   Никогда не передавайте в поле `event_data` пароли, номера банковских карт, паспортные данные или незахешированные e-mail адреса. Передавайте только анонимизированные технические параметры: `{ "cart_sum": 450, "screen": "cart" }`.
4. **Кэширование на стороне клиента:**  
   Клиентский SDK автоматически кэширует однажды полученный вариант пользователя в `localStorage`. Это сокращает до 70% сетевых вызовов при повторных переходах по страницам приложения.
