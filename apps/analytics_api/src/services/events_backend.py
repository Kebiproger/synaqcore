"""
Выбор хранилища событий для аналитики (EVENTS_BACKEND)
======================================================
postgres (по умолчанию) — events.tracking_events в postgres-app, базовый отчёт;
clickhouse (профиль compose `scale`) — полный отчёт с сегментами, воронкой и выбросами.

Бэкенд читается из settings при каждом вызове (а не фиксируется при импорте),
чтобы тесты могли переключать его через monkeypatch.
"""

from types import ModuleType
from typing import Any, Dict, FrozenSet, List, Optional

from src.core.config import settings
from src.services import clickhouse_aggregator, postgres_aggregator

POSTGRES = "postgres"
CLICKHOUSE = "clickhouse"

# Разделы полного отчёта /stats, которые есть только в ClickHouse
OPTIONAL_SECTIONS = ("segmentation", "funnel", "outliers")

# Какие разделы поддерживает каждый бэкенд
SUPPORTS: Dict[str, FrozenSet[str]] = {
    CLICKHOUSE: frozenset(OPTIONAL_SECTIONS),
    POSTGRES: frozenset(),
}

_IMPLEMENTATIONS: Dict[str, ModuleType] = {
    CLICKHOUSE: clickhouse_aggregator,
    POSTGRES: postgres_aggregator,
}

SEGMENTS_UNSUPPORTED_DETAIL = postgres_aggregator.SEGMENTS_UNSUPPORTED_DETAIL


def current_backend() -> str:
    """Имя активного бэкенда (значение уже проверено в Settings)."""
    return settings.EVENTS_BACKEND


def supports(section: str) -> bool:
    """Поддерживает ли активный бэкенд раздел отчёта (segmentation / funnel / outliers)."""
    return section in SUPPORTS[current_backend()]


def unsupported_sections() -> List[str]:
    """Разделы отчёта, недоступные в активном бэкенде, в фиксированном порядке."""
    return [s for s in OPTIONAL_SECTIONS if not supports(s)]


def _impl() -> ModuleType:
    return _IMPLEMENTATIONS[current_backend()]


async def fetch_basic_stats(experiment_name: str) -> dict:
    return await _impl().fetch_basic_stats(experiment_name)


async def fetch_all_metrics(
    experiment_name: str,
    goal_event: str = "purchase",
    segments: Optional[List[str]] = None,
    funnel_steps: Optional[List[str]] = None,
    guardrail_events: Optional[List[str]] = None,
) -> Dict[str, Any]:
    return await _impl().fetch_all_metrics(
        experiment_name=experiment_name,
        goal_event=goal_event,
        segments=segments,
        funnel_steps=funnel_steps,
        guardrail_events=guardrail_events,
    )
