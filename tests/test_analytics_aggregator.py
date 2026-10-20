"""Юнит-тесты валидации имён в clickhouse_aggregator analytics_api.

Модуль analytics_api загружается изолированно (как в test_analytics_auth.py):
оба сервиса используют пакет верхнего уровня `src`.
ClickHouse не вызывается: _ch_query подменяется и только собирает SQL.
"""

import logging
from unittest.mock import AsyncMock, MagicMock, patch

import httpx
import pytest
from fastapi import HTTPException

from tests._isolated_import import ANALYTICS_API_DIR, load_analytics_modules


(agg,) = load_analytics_modules("src.services.clickhouse_aggregator")

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


INJECTIONS = ["x' OR '1'='1", "a', 'b", "a\\", "bad name", "a.b", ""]


class TestFunnelSteps:

    @pytest.mark.asyncio
    @pytest.mark.parametrize("bad_step", INJECTIONS)
    async def test_invalid_step_400_and_no_query(self, monkeypatch, bad_step):
        ch = AsyncMock(return_value=[])
        monkeypatch.setattr(agg, "_ch_query", ch)
        with pytest.raises(HTTPException) as exc:
            await agg.fetch_funnel_metrics("exp", ["page_view", bad_step])
        assert exc.value.status_code == 400
        ch.assert_not_called()

    @pytest.mark.asyncio
    async def test_not_a_list_400(self, monkeypatch):
        ch = AsyncMock(return_value=[])
        monkeypatch.setattr(agg, "_ch_query", ch)
        with pytest.raises(HTTPException) as exc:
            await agg.fetch_funnel_metrics("exp", "page_view")
        assert exc.value.status_code == 400
        ch.assert_not_called()

    @pytest.mark.asyncio
    async def test_valid_steps_quoted_as_literals(self, monkeypatch):
        ch = AsyncMock(return_value=[])
        monkeypatch.setattr(agg, "_ch_query", ch)
        await agg.fetch_funnel_metrics("exp.v2", ["page_view", "add-to-cart"])
        sql = ch.await_args.args[0]
        assert "event_name = 'page_view'" in sql
        assert "event_name = 'add-to-cart'" in sql


class TestGuardrailEvents:

    @pytest.mark.asyncio
    @pytest.mark.parametrize("bad_event", INJECTIONS)
    async def test_invalid_event_400_and_no_query(self, monkeypatch, bad_event):
        ch = AsyncMock(return_value=[])
        monkeypatch.setattr(agg, "_ch_query", ch)
        with pytest.raises(HTTPException) as exc:
            await agg.fetch_guardrail_metrics("exp", ["error_occurred", bad_event])
        assert exc.value.status_code == 400
        ch.assert_not_called()

    @pytest.mark.asyncio
    async def test_valid_events_in_list(self, monkeypatch):
        ch = AsyncMock(return_value=[])
        monkeypatch.setattr(agg, "_ch_query", ch)
        await agg.fetch_guardrail_metrics("exp", ["error_occurred", "payment_failed"])
        sql = ch.await_args.args[0]
        assert "event_name IN ('error_occurred', 'payment_failed')" in sql


class TestOutlierMetricField:

    @pytest.mark.asyncio
    @pytest.mark.parametrize("field", ["revenue') OR 1=1 --", "user_id", "", "Revenue", None])
    async def test_field_outside_whitelist_400(self, monkeypatch, field):
        ch = AsyncMock(return_value=[])
        monkeypatch.setattr(agg, "_ch_query", ch)
        with pytest.raises(HTTPException) as exc:
            await agg.fetch_outlier_data("exp", metric_field=field)
        assert exc.value.status_code == 400
        ch.assert_not_called()

    @pytest.mark.asyncio
    @pytest.mark.parametrize("limit", [0, -1, "10; DROP TABLE x", True, 1.5])
    async def test_invalid_limit_400(self, monkeypatch, limit):
        ch = AsyncMock(return_value=[])
        monkeypatch.setattr(agg, "_ch_query", ch)
        with pytest.raises(HTTPException) as exc:
            await agg.fetch_outlier_data("exp", limit=limit)
        assert exc.value.status_code == 400
        ch.assert_not_called()

    @pytest.mark.asyncio
    async def test_default_field_ok(self, monkeypatch):
        ch = AsyncMock(return_value=[])
        monkeypatch.setattr(agg, "_ch_query", ch)
        await agg.fetch_outlier_data("exp")
        sql = ch.await_args.args[0]
        assert "JSONExtractRaw" not in sql
        assert "JSONExtractFloat(event_data, 'revenue')" in sql
        assert "toFloat64OrZero(JSONExtractString(event_data, 'revenue'))" in sql
        assert "LIMIT 5000" in sql


class TestFetchAllMetricsValidation:
    """Недопустимые шаги/события дают 400 до asyncio.gather (иначе ошибка утонула бы в {"error": ...})."""

    @pytest.mark.asyncio
    @pytest.mark.parametrize("kwargs", [
        {"funnel_steps": ["page_view", "x' OR '1'='1"]},
        {"guardrail_events": ["a', 'b"]},
    ])
    async def test_invalid_lists_400(self, monkeypatch, kwargs):
        ch = AsyncMock(return_value=[])
        monkeypatch.setattr(agg, "_ch_query", ch)
        with pytest.raises(HTTPException) as exc:
            await agg.fetch_all_metrics("exp", **kwargs)
        assert exc.value.status_code == 400
        ch.assert_not_called()

    @pytest.mark.asyncio
    async def test_defaults_happy_path(self, monkeypatch):
        ch = AsyncMock(return_value=[])
        monkeypatch.setattr(agg, "_ch_query", ch)
        result = await agg.fetch_all_metrics("exp.v2", segments=["platform"])
        assert not any(isinstance(v, dict) and "error" in v for v in result.values())
        assert ch.await_count == 7


class TestRevenueExpression:
    """Выручка не должна ронять запрос, если поля нет или оно строкой."""

    @pytest.mark.asyncio
    async def test_revenue_metrics_use_safe_extract(self, monkeypatch):
        ch = AsyncMock(return_value=[])
        monkeypatch.setattr(agg, "_ch_query", ch)
        await agg.fetch_revenue_metrics("exp")
        sql = ch.await_args.args[0]
        assert "JSONExtractRaw" not in sql
        assert "toFloat64(" not in sql.replace("toFloat64OrZero(", "")
        assert sql.count("JSONExtractFloat(event_data, 'revenue')") == 2

    @pytest.mark.parametrize("field", sorted(agg._OUTLIER_METRIC_FIELDS))
    def test_json_number_expression(self, field):
        expr = agg._json_number(field)
        assert f"JSONType(event_data, '{field}') = 'String'" in expr
        assert f"JSONExtractFloat(event_data, '{field}')" in expr


class TestSegments:

    @pytest.mark.parametrize("bad", [*INJECTIONS, "geo.country", None, 5])
    def test_invalid_segment_400(self, bad):
        with pytest.raises(HTTPException) as exc:
            agg._validate_segments(["platform", bad])
        assert exc.value.status_code == 400

    def test_not_a_list_400(self):
        with pytest.raises(HTTPException) as exc:
            agg._validate_segments("platform")
        assert exc.value.status_code == 400

    def test_dedupe_preserves_order(self):
        assert agg._validate_segments(["geo", "platform", "geo", "platform"]) == ["geo", "platform"]

    def test_cap(self):
        ok = [f"s{i}" for i in range(agg.MAX_SEGMENTS)]
        assert agg._validate_segments(ok + ok) == ok
        with pytest.raises(HTTPException) as exc:
            agg._validate_segments(ok + ["extra"])
        assert exc.value.status_code == 400

    @pytest.mark.asyncio
    async def test_fetch_segment_rejects_unsafe(self, monkeypatch):
        ch = AsyncMock(return_value=[])
        monkeypatch.setattr(agg, "_ch_query", ch)
        with pytest.raises(HTTPException) as exc:
            await agg.fetch_segment_metrics("exp", "platform') OR 1=1 --")
        assert exc.value.status_code == 400
        ch.assert_not_called()

    @pytest.mark.asyncio
    async def test_fetch_all_dedupes_segments(self, monkeypatch):
        ch = AsyncMock(return_value=[])
        monkeypatch.setattr(agg, "_ch_query", ch)
        result = await agg.fetch_all_metrics("exp", segments=["platform", "platform", "geo"])
        assert [k for k in result if k.startswith("segment_")] == ["segment_platform", "segment_geo"]
        assert ch.await_count == 8

    @pytest.mark.asyncio
    @pytest.mark.parametrize("segments", [
        ["platform", "a' OR '1'='1"],
        [f"s{i}" for i in range(11)],
    ])
    async def test_fetch_all_bad_segments_400_before_queries(self, monkeypatch, segments):
        ch = AsyncMock(return_value=[])
        monkeypatch.setattr(agg, "_ch_query", ch)
        with pytest.raises(HTTPException) as exc:
            await agg.fetch_all_metrics("exp", segments=segments)
        assert exc.value.status_code == 400
        ch.assert_not_called()


class _FakeClient:
    """Подмена httpx.AsyncClient: возвращает заранее заданный ответ."""

    def __init__(self, response):
        self._response = response

    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        return False

    async def post(self, *args, **kwargs):
        return self._response


class TestErrorLeakage:
    SECRET_BODY = "Code: 60. DB::Exception: Table synthetic_db.secret_table does not exist"

    @pytest.mark.asyncio
    async def test_ch_query_hides_body_and_logs_it(self, caplog):
        response = MagicMock(status_code=500, text=self.SECRET_BODY)
        with patch.object(agg.httpx, "AsyncClient", lambda: _FakeClient(response)), \
             caplog.at_level(logging.ERROR, logger=agg.logger.name):
            with pytest.raises(HTTPException) as exc:
                await agg._ch_query("SELECT 1")
        assert exc.value.status_code == 502
        assert "secret_table" not in str(exc.value.detail)
        assert "secret_table" in caplog.text

    @pytest.mark.asyncio
    async def test_ch_query_parses_rows(self):
        response = MagicMock(status_code=200, text='{"a": 1}\n{"a": 2}\n')
        with patch.object(agg.httpx, "AsyncClient", lambda: _FakeClient(response)):
            assert await agg._ch_query("SELECT a") == [{"a": 1}, {"a": 2}]

    @pytest.mark.asyncio
    async def test_fetch_all_metrics_generic_error_marker(self, monkeypatch, caplog):
        async def failing(query):
            raise httpx.ConnectError(self.SECRET_BODY)

        monkeypatch.setattr(agg, "_ch_query", failing)
        with caplog.at_level(logging.WARNING, logger=agg.logger.name):
            result = await agg.fetch_all_metrics("exp", segments=["platform"])
        assert result["conversion_rates"] == {"error": "Не удалось получить данные"}
        assert all("secret_table" not in str(v) for v in result.values())
        assert "secret_table" in caplog.text
