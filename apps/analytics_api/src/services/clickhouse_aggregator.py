"""
ClickHouse Aggregator — сбор всех аналитических агрегатов для A/B теста
=======================================================================
ClickHouse делает то, что умеет лучше всего:
  - Быстрая агрегация миллионов событий
  - Группировка по вариантам, сегментам, временным окнам
  - Вычисление квантилей для обнаружения выбросов
  - Воронки через countDistinct на каждом шаге

Python (анализаторы) делает то, что не умеет ClickHouse:
  - Статистические тесты (p-value, Z-test, доверительные интервалы)
  - Байесовские симуляции (P(B > A), Expected Loss)
  - NLP-рекомендации на русском языке

Все запросы защищены от SQL-инъекций (CWE-89): имена эксперимента и событий
проверяются по regex, поле метрики — по белому списку, сегменты проверяются
по regex и дополнительно экранируются.
"""

import asyncio
import json as jason
import logging
import re
from typing import Any, Dict, List, Optional, Tuple

import httpx
from fastapi import HTTPException

from src.core.config import settings

logger = logging.getLogger(__name__)

# ──────────────────────────────────────────────
#  ClickHouse connection
# ──────────────────────────────────────────────

CLICKHOUSE_HTTP = (
    f"http://{settings.CLICKHOUSE_HOST}:{settings.CLICKHOUSE_HTTP_PORT}"
)
CLICKHOUSE_USER = settings.CLICKHOUSE_USER
CLICKHOUSE_PASS = settings.CLICKHOUSE_PASSWORD

# Имена экспериментов: то же правило, что в synaq (apps/synaq/internal/api/validate).
# Имя подставляется в SQL только внутри строкового литерала '...', никогда как
# идентификатор; набор символов исключает кавычки, обратный слеш и пробелы (CWE-89).
_EXPERIMENT_NAME = re.compile(r"[A-Za-z0-9_.\-]{1,50}")
# Имена событий (goal_event) — строже: без точки, 1–50 символов (как Query max_length).
_SAFE_NAME = re.compile(r"[A-Za-z0-9_\-]{1,50}")
# Поля event_data, по которым разрешён поиск выбросов. metric_field подставляется
# в SQL, поэтому принимается только из явного белого списка (CWE-89).
_OUTLIER_METRIC_FIELDS = frozenset({"revenue", "amount", "price", "quantity"})
# Максимум сегментов в одном запросе: каждый сегмент — отдельный запрос в ClickHouse.
MAX_SEGMENTS = 10


async def _ch_query(query: str) -> List[dict]:
    """Выполнить запрос к ClickHouse через HTTP, вернуть список dict.

    Всегда добавляет FORMAT JSONEachRow.
    Query string безопасна — параметры экранируются на уровне вызывающего кода.
    """
    query = query.rstrip().rstrip(";") + "\nFORMAT JSONEachRow"
    async with httpx.AsyncClient() as client:
        response = await client.post(
            CLICKHOUSE_HTTP,
            data=query,
            auth=(CLICKHOUSE_USER, CLICKHOUSE_PASS),
            timeout=15,
        )
        if response.status_code != 200:
            # Тело ответа ClickHouse (текст запроса, имена таблиц, версия) — только
            # в лог сервера; клиенту отдаём общее сообщение без деталей.
            logger.error(
                "ClickHouse вернул HTTP %s: %s",
                response.status_code, response.text[:2000],
            )
            raise HTTPException(
                status_code=502,
                detail="Ошибка хранилища аналитики",
            )
        if not response.text.strip():
            return []
        return [
            jason.loads(line)
            for line in response.text.strip().split("\n")
            if line.strip()
        ]


def _validate_experiment_name(name: str) -> None:
    """Проверка имени эксперимента: 1–50 символов, латиница, цифры, '_', '.', '-'.

    fullmatch (а не match + '$'), чтобы не пропустить завершающий перевод строки.
    Защита от SQL-инъекций (CWE-89).
    """
    if not isinstance(name, str) or not _EXPERIMENT_NAME.fullmatch(name):
        raise HTTPException(
            status_code=400,
            detail="Недопустимое имя эксперимента. "
                   "Используйте 1–50 символов: латиницу, цифры, точки, дефисы и подчёркивания.",
        )


def _validate_goal_event(goal_event: str) -> None:
    """Проверка имени целевого события: только безопасные [a-zA-Z0-9_-].

    Защита от SQL-инъекций (CWE-89).
    """
    if not isinstance(goal_event, str) or not _SAFE_NAME.fullmatch(goal_event):
        raise HTTPException(
            status_code=400,
            detail="Недопустимое имя целевого события. "
                   "Используйте только латиницу, цифры, дефисы и подчёркивания.",
        )


def _validate_event_names(event_names: List[str]) -> None:
    """Проверка списка имён событий (шаги воронки, guardrail-события).

    Каждое имя проверяется тем же правилом, что и goal_event (CWE-89).
    """
    if not isinstance(event_names, (list, tuple)):
        raise HTTPException(
            status_code=400,
            detail="Ожидается список имён событий.",
        )
    for event_name in event_names:
        _validate_goal_event(event_name)


def _validate_metric_field(metric_field: str) -> None:
    """Проверка поля метрики для поиска выбросов по белому списку (CWE-89)."""
    if not isinstance(metric_field, str) or metric_field not in _OUTLIER_METRIC_FIELDS:
        raise HTTPException(
            status_code=400,
            detail="Недопустимое поле метрики. "
                   f"Допустимые значения: {', '.join(sorted(_OUTLIER_METRIC_FIELDS))}.",
        )


def _validate_limit(limit: int) -> int:
    """LIMIT подставляется в SQL числом: принимаем только положительный int."""
    if isinstance(limit, bool) or not isinstance(limit, int) or limit < 1:
        raise HTTPException(status_code=400, detail="Недопустимое значение limit.")
    return limit


def _escape_sql_string(value: str) -> str:
    """Экранирование строки для безопасной подстановки в ClickHouse SQL.

    Защита от SQL-инъекций (CWE-89).
    """
    return value.replace("\\", "\\\\").replace("'", "\\'")


def _escape_segment(segment: str) -> str:
    """Экранирование имени сегмента."""
    return _escape_sql_string(segment)


def _validate_segments(segments: List[str]) -> List[str]:
    """Проверка списка сегментов: те же символы, что у событий, без дублей, ≤ MAX_SEGMENTS.

    Имя сегмента подставляется в SQL; помимо экранирования (_escape_segment)
    допускаем только [A-Za-z0-9_-] (CWE-89). Дубли убираются с сохранением порядка.
    """
    if not isinstance(segments, (list, tuple)):
        raise HTTPException(status_code=400, detail="Ожидается список сегментов.")
    unique: List[str] = []
    for segment in segments:
        if not isinstance(segment, str) or not _SAFE_NAME.fullmatch(segment):
            raise HTTPException(
                status_code=400,
                detail="Недопустимое имя сегмента. "
                       "Используйте только латиницу, цифры, дефисы и подчёркивания.",
            )
        if segment not in unique:
            unique.append(segment)
    if len(unique) > MAX_SEGMENTS:
        raise HTTPException(
            status_code=400,
            detail=f"Слишком много сегментов: максимум {MAX_SEGMENTS}.",
        )
    return unique


def _json_number(field: str) -> str:
    """SQL-выражение: числовое значение поля event_data, 0 при отсутствии.

    toFloat64(JSONExtractRaw(...)) падает, если поля нет ('') или значение —
    JSON-строка ('"12.5"'). JSONExtractFloat возвращает 0 для отсутствующего поля
    и числа читает как есть; строковое значение разбираем через toFloat64OrZero.
    field должен быть заранее проверен (белый список / константа).
    """
    return (
        f"if(JSONType(event_data, '{field}') = 'String', "
        f"toFloat64OrZero(JSONExtractString(event_data, '{field}')), "
        f"JSONExtractFloat(event_data, '{field}'))"
    )


# ──────────────────────────────────────────────
#  1. Базовый анализ (Core Metrics)
# ──────────────────────────────────────────────

async def fetch_basic_stats(experiment_name: str) -> dict:
    """Агрегированная сводка по событиям эксперимента для вкладки Базовая статистика."""
    _validate_experiment_name(experiment_name)

    summary = await _ch_query(f"""
        SELECT
            variant,
            count() AS total_events,
            countDistinct(user_id) AS unique_users,
            toString(min(timestamp)) AS first_event,
            toString(max(timestamp)) AS last_event
        FROM tracking_events
        WHERE experiment_name = '{experiment_name}'
        GROUP BY variant
        ORDER BY variant
    """)

    by_event = await _ch_query(f"""
        SELECT
            variant,
            event_name,
            count() AS cnt,
            countDistinct(user_id) AS users
        FROM tracking_events
        WHERE experiment_name = '{experiment_name}'
        GROUP BY variant, event_name
        ORDER BY variant, event_name
    """)

    funnel = await _ch_query(f"""
        SELECT
            pv.variant AS variant,
            pv.views AS views,
            COALESCE(cc.clicks, 0) AS clicks,
            round(COALESCE(cc.clicks, 0) / if(pv.views = 0, 1, pv.views) * 100, 1) AS conversion_pct
        FROM (
            SELECT variant, countDistinct(user_id) AS views
            FROM tracking_events
            WHERE experiment_name = '{experiment_name}' AND event_name = 'page_view'
            GROUP BY variant
        ) pv
        LEFT JOIN (
            SELECT variant, countDistinct(user_id) AS clicks
            FROM tracking_events
            WHERE experiment_name = '{experiment_name}' AND event_name = 'cta_click'
            GROUP BY variant
        ) cc ON pv.variant = cc.variant
        ORDER BY pv.variant
    """)

    recent = await _ch_query(f"""
        SELECT toString(timestamp) AS timestamp, user_id, event_name, variant, event_data
        FROM tracking_events
        WHERE experiment_name = '{experiment_name}'
        ORDER BY timestamp DESC
        LIMIT 20
    """)

    return {
        "experiment": experiment_name,
        "summary": summary,
        "by_event": by_event,
        "funnel": funnel,
        "recent": recent,
    }


async def fetch_base_metrics(experiment_name: str) -> List[dict]:
    """Базовые метрики: пользователи, события, конверсии по вариантам.

    ClickHouse запрос — один проход по таблице.
    """
    _validate_experiment_name(experiment_name)

    return await _ch_query(f"""
        SELECT
            variant,
            countDistinct(user_id) AS unique_users,
            count() AS total_events,

            -- Конверсии: считаем пользователей, у которых было событие 'purchase'
            -- (или другое целевое — определяется на уровне приложения)
            countDistinct(if(event_name = 'purchase', user_id, NULL)) AS converting_users

        FROM tracking_events
        WHERE experiment_name = '{experiment_name}'
        GROUP BY variant
        ORDER BY variant
    """)


async def fetch_conversion_rates(
    experiment_name: str,
    goal_event: str = "purchase",
) -> List[dict]:
    """Конверсии по вариантам для указанного целевого события.

    Считает:
      - Сколько уникальных пользователей было в варианте
      - Сколько из них сделали целевое действие
      - Конверсия = converting / total * 100

    Args:
        experiment_name: имя эксперимента
        goal_event: целевое событие (purchase, cta_click, signup, и т.д.)
    """
    _validate_experiment_name(experiment_name)
    _validate_goal_event(goal_event)

    return await _ch_query(f"""
        SELECT
            variant,
            countDistinct(user_id) AS total_users,
            countDistinct(if(event_name = '{goal_event}', user_id, NULL)) AS converting_users,
            round(
                countDistinct(if(event_name = '{goal_event}', user_id, NULL))
                / countDistinct(user_id) * 100,
                4
            ) AS conversion_rate_pct
        FROM tracking_events
        WHERE experiment_name = '{experiment_name}'
        GROUP BY variant
        ORDER BY variant
    """)


# ──────────────────────────────────────────────
#  2. Бизнес-анализ (Revenue / ARPU)
# ──────────────────────────────────────────────

async def fetch_revenue_metrics(experiment_name: str) -> List[dict]:
    """Выручка и ARPU по вариантам.

    Ожидается, что событие 'purchase' содержит revenue в event_data.
    """
    _validate_experiment_name(experiment_name)
    revenue = _json_number("revenue")

    return await _ch_query(f"""
        SELECT
            variant,
            countDistinct(user_id) AS total_users,
            countDistinct(if(event_name = 'purchase', user_id, NULL)) AS paying_users,
            sum(if(event_name = 'purchase', {revenue}, 0)) AS total_revenue,
            round(
                sum(if(event_name = 'purchase', {revenue}, 0))
                / countDistinct(user_id), 6
            ) AS arpu
        FROM tracking_events
        WHERE experiment_name = '{experiment_name}'
        GROUP BY variant
        ORDER BY variant
    """)


# ──────────────────────────────────────────────
#  3. Сегментация (Subgroup Analysis)
# ──────────────────────────────────────────────

async def fetch_segment_metrics(
    experiment_name: str,
    segment_field: str = "platform",
    goal_event: str = "purchase",
) -> List[dict]:
    """Конверсии по сегментам внутри каждого варианта.

    Пример: platform = 'iOS' vs 'Android' внутри variant = 'A' и 'B'.

    Args:
        experiment_name: имя эксперимента
        segment_field: поле в event_data для сегментации
                      (platform, user_tenure, traffic_source, geo, и т.д.)
        goal_event: целевое событие
    """
    _validate_experiment_name(experiment_name)
    _validate_goal_event(goal_event)
    _validate_segments([segment_field])
    escaped_segment = _escape_segment(segment_field)

    # Безопасное имя поля — экранируем через JSONExtractString
    return await _ch_query(f"""
        SELECT
            variant,
            JSONExtractString(event_data, '{escaped_segment}') AS segment_value,
            countDistinct(user_id) AS total_users,
            countDistinct(if(event_name = '{goal_event}', user_id, NULL)) AS converting_users,
            round(
                countDistinct(if(event_name = '{goal_event}', user_id, NULL))
                / countDistinct(user_id) * 100,
                4
            ) AS conversion_rate_pct
        FROM tracking_events
        WHERE experiment_name = '{experiment_name}'
          AND JSONExtractString(event_data, '{escaped_segment}') != ''
        GROUP BY variant, segment_value
        ORDER BY variant, segment_value
    """)


# ──────────────────────────────────────────────
#  4. Воронка (Funnel Analysis)
# ──────────────────────────────────────────────

async def fetch_funnel_metrics(
    experiment_name: str,
    funnel_steps: List[str],
) -> List[dict]:
    """Анализ воронки: сколько пользователей доходит до каждого шага.

    Args:
        experiment_name: имя эксперимента
        funnel_steps: упорядоченный список имён событий, составляющих воронку
                      Например: ['page_view', 'add_to_cart', 'checkout_start', 'purchase']
    """
    _validate_experiment_name(experiment_name)
    _validate_event_names(funnel_steps)

    if not funnel_steps:
        return []

    step_queries = []
    for step_index, step_name in enumerate(funnel_steps):
        step_queries.append(f"""
            SELECT
                variant,
                {step_index} AS step_order,
                '{step_name}' AS step_name,
                countDistinct(user_id) AS users_reaching
            FROM tracking_events
            WHERE experiment_name = '{experiment_name}'
              AND event_name = '{step_name}'
            GROUP BY variant
        """)

    union_query = " UNION ALL ".join(step_queries)

    return await _ch_query(f"""
        SELECT
            step_order,
            step_name,
            variant,
            users_reaching,
            -- Общее число пользователей, которые дошли до этого шага
            -- (относительно первого шага воронки)
            max(users_reaching) OVER (PARTITION BY variant) AS total_users_start
        FROM ({union_query})
        ORDER BY step_order, variant
    """)


# ──────────────────────────────────────────────
#  5. Поиск выбросов (Outlier Detection)
# ──────────────────────────────────────────────

async def fetch_outlier_data(
    experiment_name: str,
    metric_field: str = "revenue",
    limit: int = 5000,
) -> List[dict]:
    """Данные для обнаружения выбросов — значения метрики на пользователя.

    Возвращает user_id и сумму метрики для каждого пользователя.
    """
    _validate_experiment_name(experiment_name)
    _validate_metric_field(metric_field)
    limit = _validate_limit(limit)

    return await _ch_query(f"""
        SELECT
            user_id,
            sum({_json_number(metric_field)}) AS metric_sum
        FROM tracking_events
        WHERE experiment_name = '{experiment_name}'
          AND event_name = 'purchase'
        GROUP BY user_id
        ORDER BY metric_sum DESC
        LIMIT {limit}
    """)


async def fetch_user_event_counts(
    experiment_name: str,
    limit: int = 5000,
) -> List[dict]:
    """Количество событий на пользователя — для поиска ботов/спайков."""
    _validate_experiment_name(experiment_name)
    limit = _validate_limit(limit)

    return await _ch_query(f"""
        SELECT
            user_id,
            count() AS event_count
        FROM tracking_events
        WHERE experiment_name = '{experiment_name}'
        GROUP BY user_id
        ORDER BY event_count DESC
        LIMIT {limit}
    """)


# ──────────────────────────────────────────────
#  6. Guardrail метрики (метрики здоровья)
# ──────────────────────────────────────────────

async def fetch_guardrail_metrics(
    experiment_name: str,
    guardrail_events: List[str],
) -> List[dict]:
    """Метрики здоровья — частота нецелевых событий.

    Например: error_count, support_request, page_load_slow.
    """
    _validate_experiment_name(experiment_name)
    _validate_event_names(guardrail_events)

    if not guardrail_events:
        return []

    event_filter = "', '".join(guardrail_events)

    return await _ch_query(f"""
        SELECT
            variant,
            event_name,
            countDistinct(user_id) AS affected_users,
            count() AS total_occurrences
        FROM tracking_events
        WHERE experiment_name = '{experiment_name}'
          AND event_name IN ('{event_filter}')
        GROUP BY variant, event_name
        ORDER BY variant, event_name
    """)


# ──────────────────────────────────────────────
#  7. Временной ряд (тренд конверсии по дням)
# ──────────────────────────────────────────────

async def fetch_daily_trend(
    experiment_name: str,
    goal_event: str = "purchase",
) -> List[dict]:
    """Конверсия по дням — чтобы увидеть, как эффект меняется со временем."""
    _validate_experiment_name(experiment_name)
    _validate_goal_event(goal_event)

    return await _ch_query(f"""
        SELECT
            variant,
            toDate(timestamp) AS day,
            countDistinct(user_id) AS daily_users,
            countDistinct(if(event_name = '{goal_event}', user_id, NULL)) AS daily_conversions,
            round(
                countDistinct(if(event_name = '{goal_event}', user_id, NULL))
                / countDistinct(user_id) * 100, 4
            ) AS daily_conversion_rate_pct
        FROM tracking_events
        WHERE experiment_name = '{experiment_name}'
        GROUP BY variant, day
        ORDER BY day, variant
    """)



# ──────────────────────────────────────────────
#  Универсальный агрегатор (всё в одном)
# ──────────────────────────────────────────────

async def fetch_all_metrics(
    experiment_name: str,
    goal_event: str = "purchase",
    segments: Optional[List[str]] = None,
    funnel_steps: Optional[List[str]] = None,
    guardrail_events: Optional[List[str]] = None,
) -> Dict[str, Any]:
    """Собрать все метрики эксперимента одним вызовом.

    Это основной метод, который использует API-эндпоинт.
    ClickHouse выполняет ~6-8 запросов параллельно (через asyncio.gather).

    Returns:
        Словарь со всеми агрегатами для передачи в анализаторы.
    """
    _validate_experiment_name(experiment_name)
    _validate_goal_event(goal_event)

    segments = segments or ["platform", "user_tenure", "traffic_source"]
    funnel_steps = funnel_steps or [
        "page_view", "add_to_cart", "checkout_start", "purchase",
    ]
    guardrail_events = guardrail_events or [
        "error_occurred", "support_request", "payment_failed",
    ]
    # Проверяем до asyncio.gather: там исключения превращаются в {"error": ...},
    # а недопустимый ввод должен давать 400 вызывающему.
    _validate_event_names(funnel_steps)
    _validate_event_names(guardrail_events)
    segments = _validate_segments(segments)

    # Запускаем все запросы параллельно
    tasks = {
        "base_metrics": fetch_base_metrics(experiment_name),
        "conversion_rates": fetch_conversion_rates(experiment_name, goal_event),
        "revenue_metrics": fetch_revenue_metrics(experiment_name),
        "daily_trend": fetch_daily_trend(experiment_name, goal_event),
        "guardrail_metrics": fetch_guardrail_metrics(experiment_name, guardrail_events),
        "funnel_metrics": fetch_funnel_metrics(experiment_name, funnel_steps),
    }

    # Добавляем запросы по сегментам
    for segment_field in segments:
        tasks[f"segment_{segment_field}"] = fetch_segment_metrics(
            experiment_name, segment_field, goal_event,
        )

    # Выполняем все параллельно
    results = await asyncio.gather(*tasks.values(), return_exceptions=True)

    # Собираем результат в словарь
    aggregated = {}
    for task_name, result in zip(tasks.keys(), results):
        if isinstance(result, Exception):
            # Текст исключения (SQL, ответ ClickHouse) — только в лог сервера.
            logger.warning(
                "Запрос %s для эксперимента %s завершился ошибкой",
                task_name, experiment_name, exc_info=result,
            )
            aggregated[task_name] = {"error": "Не удалось получить данные"}
        else:
            aggregated[task_name] = result

    return aggregated