/**
 * SynaqCore Client SDK (Zero-dependency)
 * Легковесный SDK для стартапов и enterprise: A/B тестирование и трекинг метрик
 */

class SynaqClient {
  /**
   * @param {Object} [options]
   * @param {string} [options.baseUrl='http://localhost:8080'] URL шлюза SynaqCore
   * @param {string} [options.userId] Идентификатор пользователя (если не передан, генерируется и сохраняется в localStorage)
   * @param {boolean} [options.enableCache=true] Кэшировать ли выданные варианты локально
   * @param {number} [options.timeoutMs=2000] Таймаут запросов в миллисекундах
   * @param {string} [options.fallbackVariant='control'] Вариант по умолчанию при сбоях сети
   * @param {number} [options.bufferIntervalMs=1000] Интервал сброса буфера событий в мс (0 для мгновенной отправки)
   * @param {number} [options.maxBufferSize=10] Максимальный размер буфера событий перед принудительным сбросом
   */
  constructor(options = {}) {
    this.baseUrl = (options.baseUrl || '').replace(/\/$/, '');
    this.enableCache = options.enableCache !== false;
    this.timeoutMs = typeof options.timeoutMs === 'number' ? options.timeoutMs : 2000;
    this.fallbackVariant = options.fallbackVariant || 'control';
    this.bufferIntervalMs = typeof options.bufferIntervalMs === 'number' ? options.bufferIntervalMs : 1000;
    this.maxBufferSize = typeof options.maxBufferSize === 'number' ? options.maxBufferSize : 10;
    this.userId = options.userId || this._getOrCreateUserId();
    this._cache = {};
    this._eventBuffer = [];
    this._flushTimer = null;

    // Автоматический сброс буфера при закрытии страницы или уходе
    if (typeof window !== 'undefined') {
      const handleUnload = () => {
        this.flush({ useBeacon: true });
      };
      if (window.addEventListener) {
        window.addEventListener('beforeunload', handleUnload);
        window.addEventListener('pagehide', handleUnload);
      }
    }
  }

  /**
   * Получить текущий ID пользователя
   * @returns {string}
   */
  getUserId() {
    return this.userId;
  }

  /**
   * Генерация анонимного ID пользователя
   * @private
   */
  _getOrCreateUserId() {
    if (typeof window !== 'undefined' && window.localStorage) {
      try {
        const stored = localStorage.getItem('synaq_user_id');
        if (stored) return stored;
        const newId = 'usr_' + Math.random().toString(36).substring(2, 11) + Date.now().toString(36);
        localStorage.setItem('synaq_user_id', newId);
        return newId;
      } catch (e) {
        // Защита от режима Private Browsing с заблокированным localStorage
      }
    }
    return 'usr_anon_' + Math.random().toString(36).substring(2, 11);
  }

  /**
   * Очистить локальный кэш вариантов
   */
  clearCache() {
    this._cache = {};
    if (typeof window !== 'undefined' && window.localStorage) {
      try {
        const keysToRemove = [];
        for (let i = 0; i < localStorage.length; i++) {
          const key = localStorage.key(i);
          if (key && key.startsWith('synaq_exp_')) {
            keysToRemove.push(key);
          }
        }
        keysToRemove.forEach(k => localStorage.removeItem(k));
      } catch (e) {
        // ignore
      }
    }
  }

  /**
   * Получить вариант эксперимента для пользователя
   * @param {string} experimentName Имя эксперимента (например: 'pricing_v2')
   * @param {string} [overrideUserId] Опциональный ID пользователя
   * @returns {Promise<string>} Название варианта (например: 'control', 'variant_b')
   */
  async getVariant(experimentName, overrideUserId = null) {
    if (!experimentName) return this.fallbackVariant;
    const uid = overrideUserId || this.userId;
    const cacheKey = `synaq_exp_${experimentName}_${uid}`;

    if (this.enableCache) {
      if (this._cache[cacheKey]) return this._cache[cacheKey];
      if (typeof window !== 'undefined' && window.localStorage) {
        try {
          const cached = localStorage.getItem(cacheKey);
          if (cached) {
            this._cache[cacheKey] = cached;
            return cached;
          }
        } catch (e) {
          // ignore
        }
      }
    }

    try {
      const controller = typeof AbortController !== 'undefined' ? new AbortController() : null;
      const timer = controller ? setTimeout(() => controller.abort(), this.timeoutMs) : null;

      const url = `${this.baseUrl}/api/v1/variants/${encodeURIComponent(experimentName)}?user_id=${encodeURIComponent(uid)}`;
      const res = await fetch(url, {
        signal: controller ? controller.signal : undefined
      });
      if (timer) clearTimeout(timer);

      if (!res.ok) {
        return this.fallbackVariant;
      }
      const data = await res.json();
      const variant = data.variant || this.fallbackVariant;

      if (this.enableCache) {
        this._cache[cacheKey] = variant;
        if (typeof window !== 'undefined' && window.localStorage) {
          try {
            localStorage.setItem(cacheKey, variant);
          } catch (e) {
            // ignore
          }
        }
      }
      return variant;
    } catch (e) {
      console.warn('[SynaqCore] Сбой получения варианта, fallback на', this.fallbackVariant, e);
      return this.fallbackVariant;
    }
  }

  /**
   * Пакетное получение вариантов для нескольких тестов одновременно
   * Использует нативный эндпоинт POST /api/v1/variants/batch с fallback на параллельные вызовы
   * @param {string[]} experimentNames Список имён тестов
   * @param {string} [overrideUserId] Опциональный ID пользователя
   * @returns {Promise<Record<string, string>>}
   */
  async getVariantsBatch(experimentNames, overrideUserId = null) {
    if (!Array.isArray(experimentNames) || experimentNames.length === 0) {
      return {};
    }

    const uid = overrideUserId || this.userId;
    const results = {};
    const missing = [];

    // 1. Проверяем кэш
    for (const name of experimentNames) {
      const cacheKey = `synaq_exp_${name}_${uid}`;
      let cached = this.enableCache ? this._cache[cacheKey] : null;
      if (!cached && this.enableCache && typeof window !== 'undefined' && window.localStorage) {
        try {
          cached = localStorage.getItem(cacheKey);
          if (cached) this._cache[cacheKey] = cached;
        } catch (e) {
          // ignore
        }
      }
      if (cached) {
        results[name] = cached;
      } else {
        missing.push(name);
      }
    }

    // Если всё есть в кэше — возвращаем мгновенно
    if (missing.length === 0) {
      return results;
    }

    // 2. Запрашиваем отсутствующие через нативный batch API
    try {
      const controller = typeof AbortController !== 'undefined' ? new AbortController() : null;
      const timer = controller ? setTimeout(() => controller.abort(), this.timeoutMs) : null;

      const res = await fetch(`${this.baseUrl}/api/v1/variants/batch`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ user_id: uid, experiments: missing }),
        signal: controller ? controller.signal : undefined
      });
      if (timer) clearTimeout(timer);

      if (res.ok) {
        const data = await res.json();
        const serverVariants = data.variants || {};
        for (const name of missing) {
          const variant = serverVariants[name] || this.fallbackVariant;
          results[name] = variant;
          if (this.enableCache) {
            const cacheKey = `synaq_exp_${name}_${uid}`;
            this._cache[cacheKey] = variant;
            if (typeof window !== 'undefined' && window.localStorage) {
              try { localStorage.setItem(cacheKey, variant); } catch (e) {}
            }
          }
        }
        return results;
      }
    } catch (e) {
      // сетевой сбой — выполняем fallback ниже
    }

    // 3. Fallback: параллельные одиночные запросы
    const promises = missing.map(async (name) => {
      results[name] = await this.getVariant(name, uid);
    });
    await Promise.all(promises);
    return results;
  }

  /**
   * Отправить событие конверсии или трекинга
   * @param {string} eventName Имя события (например: 'signup', 'purchase', 'cta_click')
   * @param {Object} [details] Дополнительные параметры
   * @param {string} [details.experimentName]
   * @param {string} [details.variant]
   * @param {Object} [details.data] Любые JSON данные (например: { amount: 99.9 })
   * @param {string} [details.userId]
   * @param {string} [details.eventId]
   * @param {boolean} [details.useBeacon=false] Использовать navigator.sendBeacon
   * @param {boolean} [details.immediate=false] Немедленная отправка в обход буфера
   * @returns {Promise<boolean>}
   */
  async track(eventName, details = {}) {
    if (!eventName) return false;
    const uid = details.userId || this.userId;
    const expName = details.experimentName || '';
    let variant = details.variant || '';

    // Если передан эксперимент, но не передан вариант — пробуем взять из кэша
    if (expName && !variant) {
      const cacheKey = `synaq_exp_${expName}_${uid}`;
      variant = this._cache[cacheKey] || '';
      if (!variant && typeof window !== 'undefined' && window.localStorage) {
        try {
          variant = localStorage.getItem(cacheKey) || '';
        } catch (e) {
          // ignore
        }
      }
    }

    const payload = {
      event_id: details.eventId || details.event_id || ('evt_' + Math.random().toString(36).substring(2, 11) + Date.now().toString(36)),
      timestamp: Date.now(),
      user_id: uid,
      event_name: eventName,
      experiment_name: expName,
      variant: variant,
      event_data: details.data ? JSON.stringify(details.data) : ''
    };

    // Если запрошен beacon или немедленная отправка без буферизации
    if (details.useBeacon || details.immediate || this.bufferIntervalMs <= 0) {
      return this._sendSingleEvent(payload, details.useBeacon);
    }

    // Добавляем событие в буфер
    this._eventBuffer.push(payload);

    // Если буфер заполнен — сбрасываем немедленно
    if (this._eventBuffer.length >= this.maxBufferSize) {
      this.flush();
    } else if (!this._flushTimer) {
      // Иначе планируем таймер сброса
      this._flushTimer = setTimeout(() => {
        this._flushTimer = null;
        this.flush();
      }, this.bufferIntervalMs);
    }

    return true;
  }

  /**
   * Отправка одиночного события
   * @private
   */
  async _sendSingleEvent(payload, useBeacon = false) {
    const targetUrl = `${this.baseUrl}/api/v1/tracker/events`;
    const jsonBody = JSON.stringify(payload);

    // 1. Попытка отправки через navigator.sendBeacon
    if (useBeacon && typeof navigator !== 'undefined' && navigator.sendBeacon) {
      try {
        const blob = new Blob([jsonBody], { type: 'application/json' });
        const success = navigator.sendBeacon(targetUrl, blob);
        if (success) return true;
      } catch (e) {
        // fallback на обычный fetch
      }
    }

    // 2. Доставка через fetch c keepalive
    try {
      const controller = typeof AbortController !== 'undefined' ? new AbortController() : null;
      const timer = controller ? setTimeout(() => controller.abort(), this.timeoutMs) : null;

      const res = await fetch(targetUrl, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: jsonBody,
        keepalive: true,
        signal: controller ? controller.signal : undefined
      });
      if (timer) clearTimeout(timer);
      return res.ok;
    } catch (e) {
      console.warn('[SynaqCore] Ошибка отправки события:', e);
      return false;
    }
  }

  /**
   * Сбросить накопленный буфер событий
   * @param {Object} [options]
   * @param {boolean} [options.useBeacon=false] Использовать sendBeacon для отправки
   * @returns {Promise<boolean>}
   */
  async flush(options = {}) {
    if (this._flushTimer) {
      clearTimeout(this._flushTimer);
      this._flushTimer = null;
    }

    if (this._eventBuffer.length === 0) {
      return true;
    }

    const eventsToSend = [...this._eventBuffer];
    this._eventBuffer = [];

    const useBeacon = options.useBeacon || false;
    const promises = eventsToSend.map(event => this._sendSingleEvent(event, useBeacon));
    const results = await Promise.all(promises);
    return results.every(Boolean);
  }
}

// Экспорт для Node.js / CommonJS
if (typeof module !== 'undefined' && module.exports) {
  module.exports = SynaqClient;
  module.exports.SynaqClient = SynaqClient;
  module.exports.default = SynaqClient;
}

// Экспорт для браузера в глобальную область
if (typeof window !== 'undefined') {
  window.SynaqClient = SynaqClient;
}
