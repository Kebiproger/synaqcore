"""Общая настройка тестов: пути импорта, синтетическое окружение, сброс состояния модулей.

Все значения синтетические. Переменные окружения выставляются ДО импорта src.*:
pydantic-settings отдаёт приоритет переменным окружения перед env_file=".env",
поэтому локальный .env разработчика не влияет на тесты.
"""
import os
import sys
from pathlib import Path

import pytest

# Корень проекта (на уровень выше tests/)
PROJECT_ROOT = Path(__file__).resolve().parent.parent

# Experiment API — основной модуль под тестами (src.* → apps/experiment_api/src)
EXPERIMENT_API_SRC = PROJECT_ROOT / "apps" / "experiment_api"

if str(EXPERIMENT_API_SRC) not in sys.path:
    sys.path.insert(0, str(EXPERIMENT_API_SRC))

# ─────────── Синтетическое окружение для Settings ───────────
# Обязательные поля: setdefault, чтобы CI/разработчик мог переопределить.
_SYNTHETIC_ENV = {
    "REDIS_URL": "redis://localhost:6379/0",
    "POSTGRES_HOST": "localhost",
    "POSTGRES_PORT": "5432",
    "POSTGRES_USER": "test_user",
    "POSTGRES_PASSWORD": "test_password",
    "POSTGRES_DB": "test_db",
    "CLICKHOUSE_HOST": "localhost",
    "CLICKHOUSE_HTTP_PORT": "8123",
    "CLICKHOUSE_USER": "test_user",
    "CLICKHOUSE_PASSWORD": "test_password",
}
for _key, _value in _SYNTHETIC_ENV.items():
    os.environ.setdefault(_key, _value)

# OIDC принудительно пустой: тесты сами выставляют настройки через monkeypatch,
# значения из .env / окружения разработчика не должны просачиваться.
for _key in ("OIDC_ISSUER", "OIDC_JWKS_URL", "OIDC_JWKS_HOST", "OIDC_CONFIG_FILE", "OIDC_AUDIENCE"):
    os.environ[_key] = ""


@pytest.fixture(autouse=True)
def _reset_module_state():
    """Сброс глобального состояния между тестами: Redis circuit breaker и кэши OIDC."""
    from src.api.v1 import client as client_module
    from src.core import auth

    client_module._reset_redis_breaker()
    auth.reset_auth_caches()
    yield
    client_module._reset_redis_breaker()
    auth.reset_auth_caches()
