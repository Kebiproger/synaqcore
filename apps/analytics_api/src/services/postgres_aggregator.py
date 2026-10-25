"""
Postgres Aggregator — базовый отчёт A/B теста из events.tracking_events
=======================================================================
Хранилище событий по умолчанию (EVENTS_BACKEND=postgres): synaq пишет сырые
события в схему events того же postgres-app, дедупликация — первичным ключом
(event_id, user_id, event_name, event_date), при конфликте остаётся первая строка.

Повторяет функции и ключи строк clickhouse_aggregator для базового отчёта:
fetch_basic_stats, fetch_base_metrics, fetch_conversion_rates,
fetch_revenue_metrics, fetch_guardrail_metrics, fetch_daily_trend,
fetch_all_metrics. Сегментация, воронка и выбросы — только в ClickHouse
(профиль scale), см. events_backend.SUPPORTS.

Безопасность (CWE-89): пользовательский ввод передаётся только параметрами $n,
текст SQL — константы модуля. Ввод дополнительно проверяется теми же правилами,
что и в ClickHouse-режиме (src/services/validation.py), чтобы ответы 400 совпадали.
Роль synaq_analytics — только чтение схемы events, statement_timeout=15s.
"""

import asyncio
import logging
import math
from decimal import Decimal
from typing import Any, Dict, List, Optional

import asyncpg
from fastapi import HTTPException

from src.core.config import settings
from src.services.validation import (
    _validate_event_names,
    _validate_experiment_name,
    _validate_goal_event,
)

logger = logging.getLogger(__name__)

SEGMENTS_UNSUPPORTED_DETAIL = (
    "Сегменты доступны только при EVENTS_BACKEND=clickhouse (профиль scale)"
)

# ──────────────────────────────────────────────
#  Пул соединений
# ──────────────────────────────────────────────

# Маленький пул: роль synaq_analytics ограничена CONNECTION LIMIT 6,
# fetch_all_metrics запускает до 5 запросов параллельно (лишние ждут в очереди).
POOL_MIN_SIZE = 1
POOL_MAX_SIZE = 4
COMMAND_TIMEOUT_S = 15

_pool: Optional[asyncpg.Pool] = None
_pool_lock = asyncio.Lock()


def _pool_configured() -> bool:
    return bool(settings.EVENTS_PG_HOST and settings.EVENTS_PG_DB
                and settings.EVENTS_PG_USER and settings.EVENTS_PG_PASSWORD)


async def init_pool() -> Optional[asyncpg.Pool]:
    """Создать пул (идемпотентно). Ошибку подключения логируем и пробуем снова при запросе.

    Сервис стартует, даже если postgres-app ещё не готов: /health отвечает,
    а /stats поднимет пул при первом обращении.
    """
    global _pool
    async with _pool_lock:
        if _pool is not None:
            return _pool
        if not _pool_configured():
            logger.error(
                "EVENTS_BACKEND=postgres, но не заданы EVENTS_PG_HOST/EVENTS_PG_DB/"
                "EVENTS_PG_USER/EVENTS_PG_PASSWORD — /stats недоступен",
            )
            return None
        try:
            _pool = await asyncpg.create_pool(
                host=settings.EVENTS_PG_HOST,
                port=settings.EVENTS_PG_PORT,
                database=settings.EVENTS_PG_DB,
                user=settings.EVENTS_PG_USER,
                password=settings.EVENTS_PG_PASSWORD,
                min_size=POOL_MIN_SIZE,
                max_size=POOL_MAX_SIZE,
                command_timeout=COMMAND_TIMEOUT_S,
                timeout=5,
            )
        except (OSError, asyncio.TimeoutError, asyncpg.PostgresError,
                asyncpg.InterfaceError) as exc:
            # Текст ошибки может содержать хост/имя роли — только в лог сервера
            logger.error("Не удалось подключиться к хранилищу событий Postgres: %r", exc)
            _pool = None
        return _pool


async def close_pool() -> None:
    """Закрыть пул при остановке сервиса."""
    global _pool
    async with _pool_lock:
        if _pool is not None:
            await _pool.close()
            _pool = None


_MAX_EXACT_INT = Decimal(2) ** 63


def _to_json_value(value: Any) -> Any:
    """Decimal (numeric из Postgres) → int / float, как числа в JSONEachRow ClickHouse."""
    if isinstance(value, Decimal):
        if not value.is_finite():
            return 0.0
        if value.as_tuple().exponent >= 0 and abs(value) < _MAX_EXACT_INT:
            return int(value)
        result = float(value)
        return result if math.isfinite(result) else 0.0
    return value


async def _pg_fetch(query: str, *args: Any) -> List[dict]:
    """Выполнить параметризованный запрос, вернуть список dict.

    query — только константный текст SQL, весь ввод — в args ($1, $2, ...).
    Ошибки БД логируются, клиенту — общее сообщение 502 без деталей.
    """
    pool = _pool or await init_pool()
    if pool is None:
        raise HTTPException(status_code=502, detail="Ошибка хранилища аналитики")
    try:
        async with pool.acquire() as conn:
            records = await conn.fetch(query, *args)
    except (asyncpg.PostgresError, asyncpg.InterfaceError, OSError, asyncio.TimeoutError) as exc:
        logger.error("Запрос к хранилищу событий Postgres завершился ошибкой: %r", exc)
        raise HTTPException(status_code=502, detail="Ошибка хранилища аналитики") from None
    return [{k: _to_json_value(v) for k, v in record.items()} for record in records]


# ──────────────────────────────────────────────
#  SQL (константы; ввод — только $n)
# ──────────────────────────────────────────────

# Время в формате toString(DateTime64(3, 'UTC')) ClickHouse: '2026-01-31 12:00:00.123'
_TS_FORMAT = "'YYYY-MM-DD HH24:MI:SS.MS'"

# Выручка события: число из event_data.revenue; JSON-строка — только если похожа
# на десятичное число с порядком ≤ 3 цифр (иначе приведение ::numeric упало бы
# или разрешило бы гигантские значения); всё остальное (нет поля, объект, bool,
# event_data не объект или NULL) — 0. Значения с |x| ≥ 1e15 тоже считаются 0:
# одно абсурдное событие не должно превращать сумму в inf и ломать JSON-ответ.
# Оператор -> для не-объекта (строка, число, массив) возвращает NULL без ошибки.
_REVENUE_RAW = r"""
            CASE jsonb_typeof(event_data -> 'revenue')
                WHEN 'number' THEN (event_data ->> 'revenue')::numeric
                WHEN 'string' THEN
                    CASE WHEN (event_data ->> 'revenue')
                              ~ '^\s*[-+]?([0-9]+(\.[0-9]*)?|\.[0-9]+)([eE][-+]?[0-9]{1,3})?\s*$'
                         THEN btrim(event_data ->> 'revenue')::numeric
                    END
            END"""

_SQL_BASIC_SUMMARY = f"""
    SELECT
        variant,
        count(*) AS total_events,
        count(DISTINCT user_id) AS unique_users,
        to_char(min(ts) AT TIME ZONE 'UTC', {_TS_FORMAT}) AS first_event,
        to_char(max(ts) AT TIME ZONE 'UTC', {_TS_FORMAT}) AS last_event
    FROM events.tracking_events
    WHERE experiment_name = $1
    GROUP BY variant
    ORDER BY variant
"""

_SQL_BASIC_BY_EVENT = """
    SELECT
        variant,
        event_name,
        count(*) AS cnt,
        count(DISTINCT user_id) AS users
    FROM events.tracking_events
    WHERE experiment_name = $1
    GROUP BY variant, event_name
    ORDER BY variant, event_name
"""

# Как в ClickHouse: варианты без page_view в воронку /basic не попадают (LEFT JOIN от pv)
_SQL_BASIC_FUNNEL = """
    SELECT
        variant,
        views,
        clicks,
        round(clicks * 100.0 / CASE WHEN views = 0 THEN 1 ELSE views END, 1) AS conversion_pct
    FROM (
        SELECT
            variant,
            count(DISTINCT user_id) FILTER (WHERE event_name = 'page_view') AS views,
            count(DISTINCT user_id) FILTER (WHERE event_name = 'cta_click') AS clicks
        FROM events.tracking_events
        WHERE experiment_name = $1 AND event_name IN ('page_view', 'cta_click')
        GROUP BY variant
    ) f
    WHERE views > 0
    ORDER BY variant
"""

# event_data отдаётся текстом, как колонка String в ClickHouse: JSON-строка
# разворачивается (#>> '{}'), объект/число сериализуются, NULL → ''.
_SQL_BASIC_RECENT = f"""
    SELECT
        to_char(ts AT TIME ZONE 'UTC', {_TS_FORMAT}) AS timestamp,
        user_id,
        event_name,
        variant,
        COALESCE(event_data #>> '{{}}', '') AS event_data
    FROM events.tracking_events
    WHERE experiment_name = $1
    ORDER BY ts DESC
    LIMIT 20
"""

_SQL_BASE_METRICS = """
    SELECT
        variant,
        count(DISTINCT user_id) AS unique_users,
        count(*) AS total_events,
        count(DISTINCT user_id) FILTER (WHERE event_name = 'purchase') AS converting_users
    FROM events.tracking_events
    WHERE experiment_name = $1
    GROUP BY variant
    ORDER BY variant
"""

_SQL_CONVERSION_RATES = """
    SELECT
        variant,
        count(DISTINCT user_id) AS total_users,
        count(DISTINCT user_id) FILTER (WHERE event_name = $2) AS converting_users,
        round(
            count(DISTINCT user_id) FILTER (WHERE event_name = $2) * 100.0
            / count(DISTINCT user_id),
            4
        ) AS conversion_rate_pct
    FROM events.tracking_events
    WHERE experiment_name = $1
    GROUP BY variant
    ORDER BY variant
"""

_SQL_REVENUE_METRICS = f"""
    WITH ev AS (
        SELECT variant, user_id, event_name,
            CASE WHEN event_name = 'purchase' THEN {_REVENUE_RAW.strip()}
            END AS raw_revenue
        FROM events.tracking_events
        WHERE experiment_name = $1
    ), rev AS (
        SELECT variant, user_id, event_name,
            CASE WHEN abs(raw_revenue) < 1e15 THEN raw_revenue ELSE 0 END AS revenue
        FROM ev
    )
    SELECT
        variant,
        count(DISTINCT user_id) AS total_users,
        count(DISTINCT user_id) FILTER (WHERE event_name = 'purchase') AS paying_users,
        COALESCE(sum(revenue) FILTER (WHERE event_name = 'purchase'), 0) AS total_revenue,
        round(
            COALESCE(sum(revenue) FILTER (WHERE event_name = 'purchase'), 0)
            / count(DISTINCT user_id),
            6
        ) AS arpu
    FROM rev
    GROUP BY variant
    ORDER BY variant
"""

_SQL_GUARDRAIL_METRICS = """
    SELECT
        variant,
        event_name,
        count(DISTINCT user_id) AS affected_users,
        count(*) AS total_occurrences
    FROM events.tracking_events
    WHERE experiment_name = $1
      AND event_name = ANY($2::text[])
    GROUP BY variant, event_name
    ORDER BY variant, event_name
"""

_SQL_DAILY_TREND = """
    SELECT
        variant,
        to_char(event_date, 'YYYY-MM-DD') AS day,
        count(DISTINCT user_id) AS daily_users,
        count(DISTINCT user_id) FILTER (WHERE event_name = $2) AS daily_conversions,
        round(
            count(DISTINCT user_id) FILTER (WHERE event_name = $2) * 100.0
            / count(DISTINCT user_id),
            4
        ) AS daily_conversion_rate_pct
    FROM events.tracking_events
    WHERE experiment_name = $1
    GROUP BY variant, event_date
    ORDER BY event_date, variant
"""


# ──────────────────────────────────────────────
#  Запросы
# ──────────────────────────────────────────────

async def fetch_basic_stats(experiment_name: str) -> dict:
    """Агрегированная сводка по событиям эксперимента для вкладки Базовая статистика."""
    _validate_experiment_name(experiment_name)

    summary = await _pg_fetch(_SQL_BASIC_SUMMARY, experiment_name)
    by_event = await _pg_fetch(_SQL_BASIC_BY_EVENT, experiment_name)
    funnel = await _pg_fetch(_SQL_BASIC_FUNNEL, experiment_name)
    recent = await _pg_fetch(_SQL_BASIC_RECENT, experiment_name)

    return {
        "experiment": experiment_name,
        "summary": summary,
        "by_event": by_event,
        "funnel": funnel,
        "recent": recent,
    }


async def fetch_base_metrics(experiment_name: str) -> List[dict]:
    """Базовые метрики: пользователи, события, покупатели (purchase) по вариантам."""
    _validate_experiment_name(experiment_name)
    return await _pg_fetch(_SQL_BASE_METRICS, experiment_name)


async def fetch_conversion_rates(
    experiment_name: str,
    goal_event: str = "purchase",
) -> List[dict]:
    """Конверсии по вариантам для указанного целевого события."""
    _validate_experiment_name(experiment_name)
    _validate_goal_event(goal_event)
    return await _pg_fetch(_SQL_CONVERSION_RATES, experiment_name, goal_event)


async def fetch_revenue_metrics(experiment_name: str) -> List[dict]:
    """Выручка и ARPU по вариантам (revenue из event_data события purchase)."""
    _validate_experiment_name(experiment_name)
    return await _pg_fetch(_SQL_REVENUE_METRICS, experiment_name)


async def fetch_guardrail_metrics(
    experiment_name: str,
    guardrail_events: List[str],
) -> List[dict]:
    """Метрики здоровья — частота нецелевых событий."""
    _validate_experiment_name(experiment_name)
    _validate_event_names(guardrail_events)

    if not guardrail_events:
        return []

    return await _pg_fetch(_SQL_GUARDRAIL_METRICS, experiment_name, list(guardrail_events))


async def fetch_daily_trend(
    experiment_name: str,
    goal_event: str = "purchase",
) -> List[dict]:
    """Конверсия по дням (UTC) — чтобы увидеть, как эффект меняется со временем."""
    _validate_experiment_name(experiment_name)
    _validate_goal_event(goal_event)
    return await _pg_fetch(_SQL_DAILY_TREND, experiment_name, goal_event)


async def fetch_all_metrics(
    experiment_name: str,
    goal_event: str = "purchase",
    segments: Optional[List[str]] = None,
    funnel_steps: Optional[List[str]] = None,
    guardrail_events: Optional[List[str]] = None,
) -> Dict[str, Any]:
    """Собрать метрики базового отчёта одним вызовом (сигнатура как у ClickHouse).

    Сегменты в Postgres-режиме не поддерживаются: явный список → 400.
    funnel_steps проверяются (для одинакового 400), но воронка не считается.
    Ключи funnel_metrics и segment_* в результате отсутствуют.
    """
    _validate_experiment_name(experiment_name)
    _validate_goal_event(goal_event)

    if segments:
        raise HTTPException(status_code=400, detail=SEGMENTS_UNSUPPORTED_DETAIL)
    if funnel_steps is not None:
        _validate_event_names(funnel_steps)
    guardrail_events = guardrail_events or [
        "error_occurred", "support_request", "payment_failed",
    ]
    # Проверяем до asyncio.gather: там исключения превращаются в {"error": ...}
    _validate_event_names(guardrail_events)

    tasks = {
        "base_metrics": fetch_base_metrics(experiment_name),
        "conversion_rates": fetch_conversion_rates(experiment_name, goal_event),
        "revenue_metrics": fetch_revenue_metrics(experiment_name),
        "daily_trend": fetch_daily_trend(experiment_name, goal_event),
        "guardrail_metrics": fetch_guardrail_metrics(experiment_name, guardrail_events),
    }

    results = await asyncio.gather(*tasks.values(), return_exceptions=True)

    aggregated: Dict[str, Any] = {}
    for task_name, result in zip(tasks.keys(), results):
        if isinstance(result, Exception):
            logger.warning(
                "Запрос %s для эксперимента %s завершился ошибкой",
                task_name, experiment_name, exc_info=result,
            )
            aggregated[task_name] = {"error": "Не удалось получить данные"}
        else:
            aggregated[task_name] = result

    return aggregated
