"""Юнит-тесты валидации имён в clickhouse_aggregator analytics_api.

Модуль analytics_api загружается изолированно (как в test_analytics_auth.py):
оба сервиса используют пакет верхнего уровня `src`.
ClickHouse не вызывается: _ch_query подменяется и только собирает SQL.
"""

import importlib
import sys
from pathlib import Path
from unittest.mock import AsyncMock

import pytest
from fastapi import HTTPException

ANALYTICS_API_DIR = str(Path(__file__).resolve().parent.parent / "apps" / "analytics_api")


def _is_src(name: str) -> bool:
    return name == "src" or name.startswith("src.")


def _load_aggregator():
    saved_modules = {k: v for k, v in sys.modules.items() if _is_src(k)}
    saved_path = list(sys.path)
    for k in saved_modules:
        del sys.modules[k]
    sys.path.insert(0, ANALYTICS_API_DIR)
    importlib.invalidate_caches()
    try:
        return importlib.import_module("src.services.clickhouse_aggregator")
    finally:
        for k in [k for k in sys.modules if _is_src(k)]:
            del sys.modules[k]
        sys.modules.update(saved_modules)
        sys.path[:] = saved_path
        importlib.invalidate_caches()


agg = _load_aggregator()

UNSAFE_NAMES = [
    "",
    "a" * 51,
    "bad'name",
    "bad\\name",
    "bad name",
    "bad\nname",
    "trailing_newline\n",
    "a;DROP",
    "a/b",
    "имя",
]


def test_module_is_from_analytics_api():
    assert agg.__file__.startswith(ANALYTICS_API_DIR)


class TestExperimentNameRule:

    @pytest.mark.parametrize("name", ["exp", "exp.v2", "a.b", "a-b_c.1", "a" * 50])
    def test_valid_names(self, name):
        agg._validate_experiment_name(name)

    @pytest.mark.parametrize("name", UNSAFE_NAMES)
    def test_invalid_names_400(self, name):
        with pytest.raises(HTTPException) as exc:
            agg._validate_experiment_name(name)
        assert exc.value.status_code == 400


class TestGoalEventRule:

    @pytest.mark.parametrize("goal", ["purchase", "cta_click", "sign-up", "a" * 50])
    def test_valid_goals(self, goal):
        agg._validate_goal_event(goal)

    @pytest.mark.parametrize("goal", ["a.b", "ns:event", *UNSAFE_NAMES])
    def test_invalid_goals_400(self, goal):
        with pytest.raises(HTTPException) as exc:
            agg._validate_goal_event(goal)
        assert exc.value.status_code == 400


class TestSqlPlacement:
    """Имя эксперимента попадает в SQL только как строковый литерал."""

    @pytest.mark.asyncio
    async def test_dotted_name_quoted_as_literal(self, monkeypatch):
        ch = AsyncMock(return_value=[])
        monkeypatch.setattr(agg, "_ch_query", ch)
        await agg.fetch_basic_stats("exp.v2")
        assert ch.await_count > 0
        for call in ch.await_args_list:
            sql = call.args[0]
            # каждое вхождение имени окружено одинарными кавычками
            assert sql.count("exp.v2") == sql.count("'exp.v2'") > 0

    @pytest.mark.asyncio
    async def test_conversion_rates_goal_and_name_literals(self, monkeypatch):
        ch = AsyncMock(return_value=[])
        monkeypatch.setattr(agg, "_ch_query", ch)
        await agg.fetch_conversion_rates("exp.v2", "purchase")
        sql = ch.await_args.args[0]
        assert "experiment_name = 'exp.v2'" in sql
        assert "event_name = 'purchase'" in sql

    @pytest.mark.asyncio
    async def test_invalid_name_never_reaches_clickhouse(self, monkeypatch):
        ch = AsyncMock(return_value=[])
        monkeypatch.setattr(agg, "_ch_query", ch)
        with pytest.raises(HTTPException):
            await agg.fetch_basic_stats("x' OR '1'='1")
        ch.assert_not_called()
