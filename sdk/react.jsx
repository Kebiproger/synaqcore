/**
 * @synaq/client React Integration
 * Легковесный провайдер и хук useVariant для React и Next.js
 */

import React, { createContext, useContext, useState, useEffect, useRef } from 'react';

const SynaqContext = createContext(null);

/**
 * SynaqProvider — контекстный провайдер для инициализации SynaqCore в React
 * @param {Object} props
 * @param {import('./synaq').SynaqClient} props.client Экземпляр SynaqClient
 * @param {React.ReactNode} props.children
 */
export function SynaqProvider({ client, children }) {
  if (!client) {
    throw new Error('[SynaqCore] SynaqProvider требует обязательный пропс client');
  }

  return (
    <SynaqContext.Provider value={client}>
      {children}
    </SynaqContext.Provider>
  );
}

/**
 * useSynaq — доступ к инстансу SynaqClient
 * @returns {import('./synaq').SynaqClient}
 */
export function useSynaq() {
  const client = useContext(SynaqContext);
  if (!client) {
    throw new Error('[SynaqCore] useSynaq должен использоваться внутри <SynaqProvider>');
  }
  return client;
}

/**
 * useVariant — хук получения варианта для React-компонентов
 * Автоматически считывает синхронно кэшированный вариант (предотвращая мигание FOOC)
 * и обновляет состояние асинхронно при необходимости.
 *
 * @param {string} experimentName Имя эксперимента
 * @param {Object} [options]
 * @param {string} [options.fallback='control'] Дефолтный вариант до ответа сервера
 * @returns {{ variant: string, isLoading: boolean, track: (eventName: string, details?: any) => Promise<boolean> }}
 */
export function useVariant(experimentName, options = {}) {
  const client = useSynaq();
  const fallback = options.fallback || client.fallbackVariant || 'control';

  // Пытаемся синхронно прочитать из кэша памяти, чтобы не было FOOC
  const initialVariant = (() => {
    if (!experimentName) return fallback;
    const cacheKey = `synaq_exp_${experimentName}_${client.getUserId()}`;
    if (client._cache && client._cache[cacheKey]) {
      return client._cache[cacheKey];
    }
    if (typeof window !== 'undefined' && window.localStorage) {
      try {
        const stored = localStorage.getItem(cacheKey);
        if (stored) return stored;
      } catch (e) {}
    }
    return fallback;
  })();

  const [variant, setVariant] = useState(initialVariant);
  const [isLoading, setIsLoading] = useState(initialVariant === fallback);
  const isMountedRef = useRef(true);

  useEffect(() => {
    isMountedRef.current = true;

    if (!experimentName) {
      setIsLoading(false);
      return;
    }

    client.getVariant(experimentName)
      .then((assigned) => {
        if (isMountedRef.current) {
          setVariant(assigned);
          setIsLoading(false);
        }
      })
      .catch(() => {
        if (isMountedRef.current) {
          setIsLoading(false);
        }
      });

    return () => {
      isMountedRef.current = false;
    };
  }, [client, experimentName]);

  const track = (eventName, details = {}) => {
    return client.track(eventName, {
      experimentName,
      variant,
      ...details
    });
  };

  return { variant, isLoading, track };
}
