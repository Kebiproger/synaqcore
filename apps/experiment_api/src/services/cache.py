import logging

import redis.asyncio as aioredis
from redis.asyncio.retry import Retry
from redis.backoff import NoBackoff
from pydantic import ValidationError
from typing import Optional
from src.schemas.experiment import ExperimentConfigCreate

logger = logging.getLogger(__name__)

# TTL ключа эксперимента: если инвалидация (delete) не прошла, устаревший
# конфиг всё равно истечёт и будет перечитан из PostgreSQL
EXPERIMENT_TTL_SECONDS = 300

# Короткие таймауты: зависший Redis не должен держать запрос дольше ~1 с
# до fallback на PostgreSQL (дефолты redis-py — 5 с и 10 повторов)
REDIS_SOCKET_TIMEOUT = 0.3
REDIS_CONNECT_TIMEOUT = 0.3
REDIS_RETRIES = 1
REDIS_HEALTH_CHECK_INTERVAL = 30


class RedisCache:
    def __init__(self, redis_url: str):
        self.redis_url = redis_url
        self.redis = None

    async def connect(self):
        self.redis = await aioredis.from_url(
            self.redis_url,
            decode_responses=True,  # Возвращает строку а не байт
            socket_timeout=REDIS_SOCKET_TIMEOUT,
            socket_connect_timeout=REDIS_CONNECT_TIMEOUT,
            retry=Retry(NoBackoff(), REDIS_RETRIES),
            health_check_interval=REDIS_HEALTH_CHECK_INTERVAL,
        )
        print("Подключение успешна к Redis!")

    @staticmethod
    def _parse(redis_key: str, raw_data: str) -> Optional[ExperimentConfigCreate]:
        """Битый JSON в кэше считаем промахом: вызывающий перезапишет ключ из БД"""
        try:
            return ExperimentConfigCreate.model_validate_json(raw_data)
        except ValidationError as e:
            logger.warning(
                "Некорректные данные в кэше по ключу %r, считаем промахом (%d ошибок валидации)",
                redis_key, e.error_count(),
            )
            return None

    async def get_experiment(self, experiment_name: str) -> Optional[ExperimentConfigCreate]:

        if not self.redis:
            raise RuntimeError("Redis не инициализирован")

        redis_key = f'experiment:{experiment_name}'

        raw_data = await self.redis.get(redis_key)

        if not raw_data:
            return None

        return self._parse(redis_key, raw_data)

    async def get_experiment_by_url(self, url: str) -> Optional[ExperimentConfigCreate]:
        """Найти эксперимент по URL-паттерну через неблокирующий итератор SCAN"""
        if not self.redis:
            raise RuntimeError("Redis не инициализирован")
        async for key in self.redis.scan_iter(match="experiment:*", count=100):
            raw = await self.redis.get(key)
            if not raw:
                continue
            exp = self._parse(key, raw)
            if exp and exp.url and url.startswith(exp.url):
                return exp
        return None

    async def save_experiment(self, experiment: ExperimentConfigCreate):
        if not self.redis:
            raise RuntimeError("Redis не инициализирован")

        redis_key = f'experiment:{experiment.name}'

        json_data = experiment.model_dump_json()  # Pydantic -> JSON

        await self.redis.set(redis_key, json_data, ex=EXPERIMENT_TTL_SECONDS)

    async def delete_experiment(self, experiment_name: str):
        if not self.redis:
            raise RuntimeError("Redis не инициализирован")
        redis_key = f"experiment:{experiment_name}"
        await self.redis.delete(redis_key)

    async def close(self):
        if self.redis:
            await self.redis.close()
