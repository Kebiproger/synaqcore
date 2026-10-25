"""Общая настройка тестов: синтетическое окружение до импорта src.*.

Под юнит-тестами только analytics_api (Python-сервис lite). Его модули грузятся
изолированно через tests/_isolated_import.py (load_analytics_modules), поэтому
в sys.path здесь ничего не добавляется.

Все значения синтетические. Переменные окружения выставляются ДО импорта src.*:
pydantic-settings отдаёт приоритет переменным окружения перед env_file=".env",
поэтому локальный .env разработчика не влияет на тесты.
"""
import os

# ─────────── Синтетическое окружение для Settings ───────────
# Обязательные поля: setdefault, чтобы CI/разработчик мог переопределить.
_SYNTHETIC_ENV = {
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

# Существующие тесты роутера рассчитаны на полный (ClickHouse) отчёт; Postgres-режим
# тесты включают сами через monkeypatch settings.EVENTS_BACKEND. Пароля БД нет:
# пул asyncpg в юнит-тестах не создаётся.
os.environ["EVENTS_BACKEND"] = "clickhouse"
for _key in ("EVENTS_PG_DB", "EVENTS_PG_PASSWORD"):
    os.environ[_key] = ""
