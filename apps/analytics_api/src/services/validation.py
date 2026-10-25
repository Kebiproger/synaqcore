"""
Проверка пользовательского ввода для агрегаторов событий
=========================================================
Общие правила для обоих хранилищ событий (ClickHouse и Postgres):
имя эксперимента, имена событий, поле метрики, limit, сегменты.

В ClickHouse значения подставляются в текст SQL (внутри строковых литералов),
поэтому правила здесь — основная защита от SQL-инъекций (CWE-89). Postgres-агрегатор
передаёт значения только параметрами $n, но проверяет их теми же правилами,
чтобы оба бэкенда одинаково отвечали 400 на недопустимый ввод.
"""

import re
from typing import List

from fastapi import HTTPException

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
