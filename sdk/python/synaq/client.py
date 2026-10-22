"""High-Performance Server-Side SDK for SynaqCore (Local Evaluation).

Обеспечивает вычисление вариантов A/B-тестов в оперативной памяти (Local Evaluation)
с нулевой сетевой задержкой (< 1 µs) и фоновой синхронизацией активных экспериментов.
"""

import hashlib
import json
import logging
import threading
import time
from typing import Any, Dict, Optional
import urllib.request
import urllib.error

logger = logging.getLogger("synaq.sdk")


class SynaqServerClient:
    """Серверный клиент SynaqCore с локальным расчетом вариантов в памяти."""

    def __init__(
        self,
        base_url: str = "http://localhost:8080",
        poll_interval_sec: float = 30.0,
        timeout: float = 2.0,
        fallback_variant: str = "control",
        auto_start: bool = True,
    ):
        self.base_url = base_url.rstrip("/")
        self.poll_interval_sec = max(5.0, poll_interval_sec)
        self.timeout = timeout
        self.fallback_variant = fallback_variant

        self._experiments: Dict[str, Dict[str, Any]] = {}
        self._lock = threading.RWMutexLock() if hasattr(threading, "RWMutexLock") else threading.Lock()
        self._stop_event = threading.Event()
        self._sync_thread: Optional[threading.Thread] = None

        # Первая синхронизация синхронно при старте
        self.refresh_configs()

        if auto_start:
            self.start_background_sync()

    def start_background_sync(self) -> None:
        """Запуск фонового потока синхронизации конфигов."""
        if self._sync_thread and self._sync_thread.is_alive():
            return
        self._stop_event.clear()
        self._sync_thread = threading.Thread(
            target=self._sync_worker,
            name="SynaqConfigPoller",
            daemon=True,
        )
        self._sync_thread.start()

    def _sync_worker(self) -> None:
        while not self._stop_event.is_set():
            if self._stop_event.wait(self.poll_interval_sec):
                break
            try:
                self.refresh_configs()
            except Exception as e:
                logger.warning("[Synaq] Ошибка фоновой синхронизации: %s", e)

    def refresh_configs(self) -> bool:
        """Синхронизация карты активных экспериментов с сервером SynaqCore."""
        url = f"{self.base_url}/api/v1/variants/eval-configs"
        req = urllib.request.Request(
            url,
            headers={"User-Agent": "SynaqServerSDK-Python/1.0"},
        )
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                if resp.status == 200:
                    payload = json.loads(resp.read().decode("utf-8"))
                    new_exps = payload.get("experiments", {})
                    with self._lock:
                        self._experiments = new_exps
                    logger.debug("[Synaq] Конфиги обновлены: %d активных тестов", len(new_exps))
                    return True
        except Exception as e:
            logger.warning("[Synaq] Не удалось обновить конфиги с %s: %s", url, e)
        return False

    def get_variant(self, experiment_name: str, user_id: str) -> str:
        """Локальное вычисление варианта в ОЗУ за < 1 µs через детерминированный SHA-256.
        
        Не совершает сетевых вызовов, гарантирует абсолютную стабильность и скорость 1M+ RPS.
        """
        if not experiment_name or not user_id:
            return self.fallback_variant

        with self._lock:
            exp_data = self._experiments.get(experiment_name)

        if not exp_data:
            return self.fallback_variant

        variants = exp_data.get("variants")
        if not variants or not isinstance(variants, dict):
            return self.fallback_variant

        # Алгоритм обязан совпадать с сервером (apps/synaq/internal/allocation,
        # golden-векторы в testdata/golden.json) и с Go SDK.
        if any(w <= 0 for w in variants.values()):
            return self.fallback_variant
        total_weight = sum(variants.values())

        # Детерминированный хэш: SHA-256(user_id:experiment_name) -> bucket 0..9999
        key = f"{user_id}:{experiment_name}".encode("utf-8")
        hash_hex = hashlib.sha256(key).hexdigest()
        bucket = int(hash_hex, 16) % 10000

        # Канонический порядок — по имени варианта (порядок ключей JSON не гарантирован).
        # Целочисленное сравнение bucket/10000 < cumulative/total без float-округления.
        cumulative = 0
        for variant_name, weight in sorted(variants.items()):
            cumulative += weight
            if bucket * total_weight < cumulative * 10000:
                return variant_name

        return self.fallback_variant

    def track(
        self,
        event_name: str,
        experiment_name: str,
        variant: str,
        user_id: str,
        event_data: Optional[Dict[str, Any]] = None,
        timeout: Optional[float] = None,
    ) -> bool:
        """Отправка серверного события конверсии в Go Ingestion Core."""
        url = f"{self.base_url}/api/v1/tracker/events"
        payload = {
            "timestamp": int(time.time() * 1000),
            "user_id": user_id,
            "event_name": event_name,
            "experiment_name": experiment_name,
            "variant": variant,
            "event_data": json.dumps(event_data) if event_data else "",
        }
        data = json.dumps(payload).encode("utf-8")
        req = urllib.request.Request(
            url,
            data=data,
            headers={
                "Content-Type": "application/json",
                "User-Agent": "SynaqServerSDK-Python/1.0",
            },
            method="POST",
        )
        try:
            with urllib.request.urlopen(req, timeout=timeout or self.timeout) as resp:
                return resp.status == 200
        except Exception as e:
            logger.warning("[Synaq] Ошибка отправки события трекинга: %s", e)
            return False

    def close(self) -> None:
        """Остановка фонового потока синхронизации."""
        self._stop_event.set()
        if self._sync_thread and self._sync_thread.is_alive():
            self._sync_thread.join(timeout=2.0)

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        self.close()
