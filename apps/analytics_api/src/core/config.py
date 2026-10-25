from typing import Literal

from pydantic import field_validator
from pydantic_settings import BaseSettings, SettingsConfigDict

class Settings(BaseSettings):
    # Хранилище событий: postgres (по умолчанию, схема events в postgres-app)
    # или clickhouse (профиль compose `scale`). Иное значение — ошибка старта.
    EVENTS_BACKEND: Literal["postgres", "clickhouse"] = "postgres"

    # Postgres-режим: роль только на чтение схемы events
    # (default_transaction_read_only=on, statement_timeout=15s)
    EVENTS_PG_HOST: str = "postgres-app"
    EVENTS_PG_PORT: int = 5432
    EVENTS_PG_DB: str = ""
    EVENTS_PG_USER: str = "synaq_analytics"
    EVENTS_PG_PASSWORD: str = ""

    CLICKHOUSE_HOST: str = "clickhouse"
    CLICKHOUSE_HTTP_PORT: int = 8123
    CLICKHOUSE_USER: str = "default"
    CLICKHOUSE_PASSWORD: str = ""

    # Zitadel OIDC: пустые значения → защищённые эндпоинты отвечают 503 (fail closed)
    OIDC_ISSUER: str = ""
    OIDC_JWKS_URL: str = ""
    OIDC_JWKS_HOST: str = ""      # Host-заголовок для JWKS-запроса по внутреннему адресу
    OIDC_CONFIG_FILE: str = ""    # JSON {issuer, project_id, client_id} от bootstrap-контейнера
    OIDC_AUDIENCE: str = ""       # переопределяет project_id из OIDC_CONFIG_FILE

    model_config = SettingsConfigDict(env_file=".env", extra="ignore")

    @field_validator("EVENTS_BACKEND", mode="before")
    @classmethod
    def _normalize_backend(cls, value):
        # "Postgres" / " clickhouse " из .env — допустимые варианты написания
        return value.strip().lower() if isinstance(value, str) else value

settings = Settings()
