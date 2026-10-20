"""Тесты обработки ошибок в stats-роутере analytics_api.

Модуль загружается изолированно (см. tests/_isolated_import.py). Эндпоинт
вызывается напрямую как функция (аутентификация покрыта в test_analytics_auth.py),
все обращения к ClickHouse подменены AsyncMock. Данные синтетические.
"""

import logging
from unittest.mock import AsyncMock, patch

import httpx
import pytest
from fastapi import HTTPException

from tests._isolated_import import load_analytics_modules

(a_stats,) = load_analytics_modules("src.api.v1.stats")

CONVERSIONS = [
    {"variant": "A", "total_users": 1000, "converting_users": 100},
    {"variant": "B", "total_users": 1000, "converting_users": 130},
]
REVENUE_ROWS = [("u1", 10.0), ("u2", 12.0), ("u3", 11.0), ("u4", 9.0), ("u5", 500.0)] * 4
EVENT_ROWS = [(f"u{i}", 3) for i in range(30)] + [("bot_1", 5000)]


async def _call(**overrides):
    kwargs = dict(
        experiment_name="exp",
        method="frequentist",
        goal_event="purchase",
        segments="platform",
        hide="",
        monthly_traffic=None,
    )
    kwargs.update(overrides)
    return await a_stats.analyze_experiment(**kwargs)


def _metrics(conversion_rates):
    return AsyncMock(return_value={"conversion_rates": conversion_rates})


def _rows(user_rows, key):
    return AsyncMock(return_value=[{"user_id": u, key: v} for u, v in user_rows])


class TestConversionErrorMarker:

    @pytest.mark.asyncio
    async def test_error_dict_gives_502(self):
        with patch.object(a_stats, "fetch_all_metrics", _metrics({"error": "Не удалось получить данные"})):
            with pytest.raises(HTTPException) as exc:
                await _call()
        assert exc.value.status_code == 502
        assert exc.value.detail == "Ошибка хранилища аналитики"

    @pytest.mark.asyncio
    async def test_empty_gives_404(self):
        with patch.object(a_stats, "fetch_all_metrics", _metrics([])):
            with pytest.raises(HTTPException) as exc:
                await _call()
        assert exc.value.status_code == 404


class TestOutlierBlocks:

    @pytest.mark.asyncio
    async def test_revenue_failure_does_not_skip_spikes(self, caplog):
        with patch.object(a_stats, "fetch_all_metrics", _metrics(CONVERSIONS)), \
             patch.object(a_stats, "fetch_outlier_data",
                          AsyncMock(side_effect=HTTPException(status_code=502))), \
             patch.object(a_stats, "fetch_user_event_counts", _rows(EVENT_ROWS, "event_count")), \
             caplog.at_level(logging.WARNING, logger=a_stats.logger.name):
            result = await _call()
        assert result["outliers"] is not None
        assert "доходу" in caplog.text

    @pytest.mark.asyncio
    async def test_spike_failure_keeps_revenue_result(self, caplog):
        with patch.object(a_stats, "fetch_all_metrics", _metrics(CONVERSIONS)), \
             patch.object(a_stats, "fetch_outlier_data", _rows(REVENUE_ROWS, "metric_sum")), \
             patch.object(a_stats, "fetch_user_event_counts",
                          AsyncMock(side_effect=httpx.ConnectError("synthetic"))), \
             caplog.at_level(logging.WARNING, logger=a_stats.logger.name):
            result = await _call()
        assert result["outliers"] is not None
        assert "спайков" in caplog.text

    @pytest.mark.asyncio
    @pytest.mark.parametrize("bad_rows", [
        [{"no_user_id": 1}],                   # KeyError
        [{"user_id": "u1", "metric_sum": "x"}],  # ValueError
        [{"user_id": "u1", "metric_sum": None}],  # TypeError
    ])
    async def test_bad_rows_are_logged_not_raised(self, bad_rows):
        with patch.object(a_stats, "fetch_all_metrics", _metrics(CONVERSIONS)), \
             patch.object(a_stats, "fetch_outlier_data", AsyncMock(return_value=bad_rows)), \
             patch.object(a_stats, "fetch_user_event_counts", AsyncMock(return_value=[])):
            result = await _call()
        assert result["outliers"] is None

    @pytest.mark.asyncio
    async def test_unexpected_error_propagates(self):
        with patch.object(a_stats, "fetch_all_metrics", _metrics(CONVERSIONS)), \
             patch.object(a_stats, "fetch_outlier_data", AsyncMock(side_effect=RuntimeError("bug"))), \
             patch.object(a_stats, "fetch_user_event_counts", AsyncMock(return_value=[])):
            with pytest.raises(RuntimeError):
                await _call()


class TestDailyTrend:

    @pytest.mark.asyncio
    @pytest.mark.parametrize("bad_trend", [{"error": "Не удалось получить данные"}, None, "oops"])
    async def test_non_list_becomes_empty(self, bad_trend, caplog):
        metrics = AsyncMock(return_value={"conversion_rates": CONVERSIONS, "daily_trend": bad_trend})
        with patch.object(a_stats, "fetch_all_metrics", metrics), \
             patch.object(a_stats, "fetch_outlier_data", AsyncMock(return_value=[])), \
             patch.object(a_stats, "fetch_user_event_counts", AsyncMock(return_value=[])), \
             caplog.at_level(logging.WARNING, logger=a_stats.logger.name):
            result = await _call()
        assert result["daily_trend"] == []
        assert "daily_trend" in caplog.text

    @pytest.mark.asyncio
    async def test_list_passes_through(self):
        trend = [{"date": "2026-01-01", "variant": "A", "users": 10}]
        metrics = AsyncMock(return_value={"conversion_rates": CONVERSIONS, "daily_trend": trend})
        with patch.object(a_stats, "fetch_all_metrics", metrics), \
             patch.object(a_stats, "fetch_outlier_data", AsyncMock(return_value=[])), \
             patch.object(a_stats, "fetch_user_event_counts", AsyncMock(return_value=[])):
            result = await _call()
        assert result["daily_trend"] == trend
