/**
 * Опции инициализации клиента SynaqCore
 */
export interface SynaqClientOptions {
  /**
   * Базовый URL шлюза SynaqCore (например: 'http://localhost:8080' или 'https://ab.company.com')
   */
  baseUrl: string;

  /**
   * Идентификатор пользователя. Если не указан, автоматически генерируется и сохраняется в localStorage.
   */
  userId?: string;

  /**
   * Включить локальное кэширование назначенных вариантов в памяти и localStorage (по умолчанию: true)
   */
  enableCache?: boolean;

  /**
   * Таймаут сетевых запросов в миллисекундах (по умолчанию: 2000 мс)
   */
  timeoutMs?: number;

  /**
   * Дефолтный вариант при сетевых сбоях или недоступности сервиса (по умолчанию: 'control')
   */
  fallbackVariant?: string;

  /**
   * Интервал сброса буфера событий в мс (по умолчанию: 1000, 0 для мгновенной отправки)
   */
  bufferIntervalMs?: number;

  /**
   * Максимальный размер буфера событий перед принудительным сбросом (по умолчанию: 10)
   */
  maxBufferSize?: number;
}

/**
 * Опции отправки аналитического события
 */
export interface TrackOptions {
  /**
   * Имя эксперимента, к которому привязано событие
   */
  experimentName?: string;

  /**
   * Вариант эксперимента (если не указан, автоматически берётся из кэша назначенных вариантов)
   */
  variant?: string;

  /**
   * Произвольные полезные данные в формате JSON (например: { price: 99.9, currency: 'USD' })
   */
  data?: Record<string, any>;

  /**
   * Переопределение ID пользователя для конкретного события
   */
  userId?: string;

  /**
   * Пользовательский ID события для предотвращения дублирования (если не указан, генерируется автоматически)
   */
  eventId?: string;

  /**
   * Использовать ли navigator.sendBeacon при наличии поддержки (полезно при переходе по ссылкам)
   */
  useBeacon?: boolean;

  /**
   * Немедленная отправка в обход буфера (по умолчанию: false)
   */
  immediate?: boolean;
}

/**
 * SynaqClient — легковесный клиентский SDK для A/B тестирования и трекинга конверсий
 */
export class SynaqClient {
  constructor(options?: SynaqClientOptions);

  /**
   * Получить текущий идентификатор пользователя (из опций или сгенерированный в localStorage)
   */
  getUserId(): string;

  /**
   * Получить назначенный вариант для эксперимента
   * @param experimentName Имя эксперимента (например: 'pricing_v2')
   * @param overrideUserId Опциональный пользовательский ID
   * @returns Имя назначенного варианта или fallback ('control') при сбое
   */
  getVariant(experimentName: string, overrideUserId?: string | null): Promise<string>;

  /**
   * Пакетное получение вариантов для нескольких экспериментов одновременно
   * @param experimentNames Список имён экспериментов (например: ['header_color', 'checkout_v2'])
   * @param overrideUserId Опциональный пользовательский ID
   * @returns Карта { experimentName: variantName }
   */
  getVariantsBatch(
    experimentNames: string[],
    overrideUserId?: string | null
  ): Promise<Record<string, string>>;

  /**
   * Отправить событие конверсии или пользовательского действия
   * @param eventName Имя события (например: 'cta_click', 'purchase', 'signup')
   * @param details Опции события (эксперимент, вариант, данные)
   * @returns true при успешной доставке, false при ошибке
   */
  track(eventName: string, details?: TrackOptions): Promise<boolean>;

  /**
   * Принудительно очистить локальный кэш вариантов
   */
  clearCache(): void;

  /**
   * Завершить работу клиента и дождаться отправки активных вызовов
   */
  flush(): Promise<boolean>;
}

export default SynaqClient;
