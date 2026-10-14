# @synaq/client — JavaScript & TypeScript Client SDK

Официальная клиентская библиотека для интеграции платформы A/B-тестирования **SynaqCore** в веб-сайты и фронтенд-приложения.

* 🚀 **Zero-dependency:** Весит менее 3 КБ, не тянет за собой чужих пакетов.
* 🛡️ **Fail-Safe Fallback:** Никогда не роняет клиентский интерфейс, при сетевых сбоях возвращает дефолтный вариант `'control'`.
* ⚡ **High Performance:** Двухуровневое кэширование (ОЗУ + `localStorage`), `keepalive: true` и поддержка `navigator.sendBeacon`.
* 📦 **Полная типизация:** Поставляется с готовыми TypeScript-декларациями `synaq.d.ts`.

---

## 📦 Установка

### Вариант 1: Через npm / pnpm / yarn
```bash
npm install @synaq/client
# или
pnpm add @synaq/client
```

### Вариант 2: Прямо со шлюза SynaqCore (без сборщиков, для лендингов)
```html
<script src="https://ab.company.com/sdk/synaq.js"></script>
```

---

## ⚡ Быстрый старт

### 1. Инициализация клиента
```typescript
import SynaqClient from '@synaq/client';

const synaq = new SynaqClient({
  baseUrl: 'https://ab.company.com', // Адрес вашего шлюза SynaqCore
  timeoutMs: 2000,                   // Таймаут сетевых запросов (по умолчанию 2000 мс)
  fallbackVariant: 'control',        // Дефолтный вариант при ошибках сети
  enableCache: true                  // Кэшировать варианты в localStorage
});
```

### 2. Получение варианта (`getVariant`)
```typescript
async function renderPage() {
  const variant = await synaq.getVariant('new_hero_banner');

  if (variant === 'variant_b') {
    showSpecialOfferBanner();
  } else {
    showDefaultBanner();
  }
}
renderPage();
```

### 3. Пакетное получение вариантов (`getVariantsBatch`)
Когда на одной странице запущено сразу несколько A/B-экспериментов:
```typescript
const variants = await synaq.getVariantsBatch([
  'hero_banner',
  'cta_button_color',
  'checkout_v2'
]);

console.log(variants);
// { hero_banner: 'B', cta_button_color: 'green', checkout_v2: 'control' }
```

### 4. Трекинг кликов и конверсий (`track`)
```typescript
document.getElementById('buy-button').addEventListener('click', () => {
  synaq.track('cta_click', {
    experimentName: 'new_hero_banner',
    data: { price: 49.99, currency: 'USD' },
    useBeacon: true // отправка через navigator.sendBeacon при уходе со страницы
  });
});
```

---

## 🛡️ Безопасность и отказоустойчивость
* **Кэширование:** Если пользователю однажды выдан вариант `variant_b`, SDK сохраняет его в памяти и `localStorage`. Повторные переходы по страницам сайта не создают лишних сетевых запросов.
* **Изоляция сбоев:** При падении сервера SynaqCore, таймауте или 5xx метод `getVariant` тихо возвращает `'control'`, а в консоли браузера выводится предупреждение. Сайт клиента всегда продолжает работать стабильно.
