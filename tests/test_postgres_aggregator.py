"""Тесты Postgres-бэкенда аналитики (EVENTS_BACKEND=postgres).

Модули analytics_api загружаются изолированно одним вызовом (общий settings),
см. tests/_isolated_import.py. Postgres не нужен: _pg_fetch подменяется и только
собирает (SQL, args). Паритетный тест с настоящей БД запускается, только если
задан SYNAQ_TEST_PG_DSN (одноразовая БД: тест создаёт схему events и удаляет её).
Все данные синтетические.
"""

import logging
import os
from datetime import datetime, timezone
from decimal import Decimal
from unittest.mock import AsyncMock, patch

import pytest
from fastapi import HTTPException

from tests._isolated_import import ANALYTICS_API_DIR, load_analytics_modules

pg, eb, ch, a_stats, cfg = load_analytics_modules(
    "src.services.postgres_aggregator",
    "src.services.events_backend",
    "src.services.clickhouse_aggregator",
    "src.api.v1.stats",
    "src.core.config",
)

INJECTIONS = [
    "x' OR '1'='1",
    "a'); DROP TABLE events.tracking_events; --",
    "zz_marker_$9",
    "a\\",
    "evil\nname",
]

SQL_CONSTANTS = {
    value for name, value in vars(pg).items() if name.startswith("_SQL_")
}


def test_modules_are_from_analytics_api():
    assert pg.__file__.startswith(ANALYTICS_API_DIR)
    assert eb.settings is cfg.settings is pg.settings


@pytest.fixture
def postgres_mode(monkeypatch):
    monkeypatch.setattr(cfg.settings, "EVENTS_BACKEND", "postgres")


@pytest.fixture
def clickhouse_mode(monkeypatch):
    monkeypatch.setattr(cfg.settings, "EVENTS_BACKEND", "clickhouse")


@pytest.fixture
def no_validation(monkeypatch):
    """Отключить проверку ввода, чтобы проверить сам механизм параметров $n."""
    for name in ("_validate_experiment_name", "_validate_goal_event", "_validate_event_names"):
        monkeypatch.setattr(pg, name, lambda *_a, **_k: None)


def _capture(monkeypatch):
    fetch = AsyncMock(return_value=[])
    monkeypatch.setattr(pg, "_pg_fetch", fetch)
    return fetch


def _assert_only_in_args(fetch, payload):
    assert fetch.await_count > 0
    for call in fetch.await_args_list:
        sql, *args = call.args
        assert sql in SQL_CONSTANTS, "SQL должен быть константой модуля"
        assert payload not in sql
        flat = [a for arg in args for a in (arg if isinstance(arg, list) else [arg])]
        assert payload in flat


class TestParametersOnly:
    """Ввод попадает только в args ($n), текст SQL — всегда константа."""

    @pytest.mark.asyncio
    @pytest.mark.parametrize("payload", INJECTIONS)
    @pytest.mark.parametrize("func", [
        "fetch_basic_stats", "fetch_base_metrics", "fetch_conversion_rates",
        "fetch_revenue_metrics", "fetch_daily_trend",
    ])
    async def test_experiment_name(self, monkeypatch, no_validation, func, payload):
        fetch = _capture(monkeypatch)
        await getattr(pg, func)(payload)
        _assert_only_in_args(fetch, payload)

    @pytest.mark.asyncio
    @pytest.mark.parametrize("payload", INJECTIONS)
    @pytest.mark.parametrize("func", ["fetch_conversion_rates", "fetch_daily_trend"])
    async def test_goal_event(self, monkeypatch, no_validation, func, payload):
        fetch = _capture(monkeypatch)
        await getattr(pg, func)("exp", payload)
        _assert_only_in_args(fetch, payload)

    @pytest.mark.asyncio
    @pytest.mark.parametrize("payload", INJECTIONS)
    async def test_guardrail_events(self, monkeypatch, no_validation, payload):
        fetch = _capture(monkeypatch)
        await pg.fetch_guardrail_metrics("exp", ["error_occurred", payload])
        _assert_only_in_args(fetch, payload)
        assert "ANY($2" in fetch.await_args.args[0]

    @pytest.mark.asyncio
    @pytest.mark.parametrize("payload", INJECTIONS)
    async def test_fetch_all_metrics(self, monkeypatch, no_validation, payload):
        fetch = _capture(monkeypatch)
        result = await pg.fetch_all_metrics(payload, goal_event=payload, guardrail_events=[payload])
        assert set(result) == {
            "base_metrics", "conversion_rates", "revenue_metrics", "daily_trend", "guardrail_metrics",
        }
        _assert_only_in_args(fetch, payload)


class TestValidation:
    """Те же 400, что и в ClickHouse-режиме; до запроса дело не доходит."""

    @pytest.mark.asyncio
    @pytest.mark.parametrize("payload", INJECTIONS)
    async def test_bad_experiment_400(self, monkeypatch, payload):
        fetch = _capture(monkeypatch)
        with pytest.raises(HTTPException) as exc:
            await pg.fetch_basic_stats(payload)
        assert exc.value.status_code == 400
        fetch.assert_not_called()

    @pytest.mark.asyncio
    @pytest.mark.parametrize("kwargs", [
        {"goal_event": "x' OR '1'='1"},
        {"guardrail_events": ["a', 'b"]},
        {"funnel_steps": ["page_view", "bad step"]},
    ])
    async def test_fetch_all_bad_input_400(self, monkeypatch, kwargs):
        fetch = _capture(monkeypatch)
        with pytest.raises(HTTPException) as exc:
            await pg.fetch_all_metrics("exp", **kwargs)
        assert exc.value.status_code == 400
        fetch.assert_not_called()

    @pytest.mark.asyncio
    async def test_fetch_all_segments_400(self, monkeypatch):
        fetch = _capture(monkeypatch)
        with pytest.raises(HTTPException) as exc:
            await pg.fetch_all_metrics("exp", segments=["platform"])
        assert exc.value.status_code == 400
        assert exc.value.detail == pg.SEGMENTS_UNSUPPORTED_DETAIL
        fetch.assert_not_called()


class TestConversionAndErrors:

    @pytest.mark.parametrize("value, expected", [
        (Decimal("12"), 12),
        (Decimal("12.5000"), 12.5),
        (Decimal("NaN"), 0.0),
        (Decimal("1e400"), 0.0),
        (Decimal("123456789012345678901234567890"), 1.2345678901234568e29),
        (7, 7),
        ("text", "text"),
        (None, None),
    ])
    def test_to_json_value(self, value, expected):
        result = pg._to_json_value(value)
        assert result == expected and type(result) is type(expected)

    @pytest.mark.asyncio
    async def test_db_error_is_502_and_logged(self, monkeypatch, caplog):
        class _Conn:
            async def fetch(self, *_a):
                raise pg.asyncpg.PostgresError("relation synthetic_secret_table does not exist")

        class _Acquire:
            async def __aenter__(self):
                return _Conn()

            async def __aexit__(self, *_exc):
                return False

        class _Pool:
            def acquire(self):
                return _Acquire()

        monkeypatch.setattr(pg, "_pool", _Pool())
        with caplog.at_level(logging.ERROR, logger=pg.logger.name):
            with pytest.raises(HTTPException) as exc:
                await pg._pg_fetch("SELECT 1")
        assert exc.value.status_code == 502
        assert exc.value.detail == "Ошибка хранилища аналитики"
        assert "synthetic_secret_table" in caplog.text

    @pytest.mark.asyncio
    async def test_unconfigured_pool_is_502(self, monkeypatch):
        monkeypatch.setattr(pg, "_pool", None)
        monkeypatch.setattr(cfg.settings, "EVENTS_PG_PASSWORD", "")
        with pytest.raises(HTTPException) as exc:
            await pg._pg_fetch("SELECT 1")
        assert exc.value.status_code == 502


class TestDispatcher:

    @pytest.mark.asyncio
    @pytest.mark.parametrize("backend, impl, other", [
        ("postgres", pg, ch),
        ("clickhouse", ch, pg),
    ])
    async def test_picks_backend(self, monkeypatch, backend, impl, other):
        monkeypatch.setattr(cfg.settings, "EVENTS_BACKEND", backend)
        for module in (impl, other):
            monkeypatch.setattr(module, "fetch_all_metrics", AsyncMock(return_value={"m": module.__name__}))
            monkeypatch.setattr(module, "fetch_basic_stats", AsyncMock(return_value={"b": module.__name__}))
        assert await eb.fetch_all_metrics("exp", goal_event="signup") == {"m": impl.__name__}
        assert await eb.fetch_basic_stats("exp") == {"b": impl.__name__}
        other.fetch_all_metrics.assert_not_called()
        other.fetch_basic_stats.assert_not_called()
        assert impl.fetch_all_metrics.await_args.kwargs["goal_event"] == "signup"

    def test_supports_flags(self, monkeypatch):
        monkeypatch.setattr(cfg.settings, "EVENTS_BACKEND", "postgres")
        assert eb.unsupported_sections() == ["segmentation", "funnel", "outliers"]
        monkeypatch.setattr(cfg.settings, "EVENTS_BACKEND", "clickhouse")
        assert eb.unsupported_sections() == []

    @pytest.mark.parametrize("raw, expected", [("Postgres", "postgres"), (" clickhouse ", "clickhouse")])
    def test_settings_normalize(self, raw, expected):
        assert cfg.Settings(EVENTS_BACKEND=raw).EVENTS_BACKEND == expected

    def test_settings_reject_unknown(self):
        with pytest.raises(ValueError):
            cfg.Settings(EVENTS_BACKEND="mysql")


CONVERSIONS = [
    {"variant": "A", "total_users": 1000, "converting_users": 100},
    {"variant": "B", "total_users": 1000, "converting_users": 130},
]


async def _call_stats(**overrides):
    kwargs = dict(
        experiment_name="exp", method="frequentist", goal_event="purchase",
        segments=None, hide="", monthly_traffic=None,
    )
    kwargs.update(overrides)
    return await a_stats.analyze_experiment(**kwargs)


class TestStatsRouterPostgresMode:

    @pytest.mark.asyncio
    @pytest.mark.parametrize("segments", ["platform", "platform,geo", " geo "])
    async def test_explicit_segments_400(self, postgres_mode, segments):
        metrics = AsyncMock()
        with patch.object(a_stats, "fetch_all_metrics", metrics):
            with pytest.raises(HTTPException) as exc:
                await _call_stats(segments=segments)
        assert exc.value.status_code == 400
        assert exc.value.detail == pg.SEGMENTS_UNSUPPORTED_DETAIL
        metrics.assert_not_called()

    @pytest.mark.asyncio
    @pytest.mark.parametrize("segments", [None, "", " , "])
    async def test_report_shape(self, postgres_mode, segments):
        metrics = AsyncMock(return_value={"conversion_rates": CONVERSIONS, "daily_trend": []})
        outliers = AsyncMock(return_value=[])
        spikes = AsyncMock(return_value=[])
        with patch.object(a_stats, "fetch_all_metrics", metrics), \
             patch.object(a_stats, "fetch_outlier_data", outliers), \
             patch.object(a_stats, "fetch_user_event_counts", spikes):
            result = await _call_stats(segments=segments)
        assert metrics.await_args.kwargs["segments"] is None
        assert result["backend"] == "postgres"
        assert result["unsupported_sections"] == ["segmentation", "funnel", "outliers"]
        assert result["segmentation"] == {}
        assert result["funnel"] is None
        assert result["outliers"] is None
        assert result["daily_trend"] == []
        assert set(result["variants"]) == {"A", "B"}
        outliers.assert_not_called()
        spikes.assert_not_called()

    @pytest.mark.asyncio
    async def test_hide_still_removes_sections(self, postgres_mode):
        metrics = AsyncMock(return_value={"conversion_rates": CONVERSIONS})
        with patch.object(a_stats, "fetch_all_metrics", metrics):
            result = await _call_stats(hide="сегменты,воронка,выбросы")
        assert "segmentation" not in result and "funnel" not in result and "outliers" not in result
        assert result["unsupported_sections"] == ["segmentation", "funnel", "outliers"]

    @pytest.mark.asyncio
    async def test_clickhouse_mode_unchanged(self, clickhouse_mode):
        metrics = AsyncMock(return_value={"conversion_rates": CONVERSIONS})
        with patch.object(a_stats, "fetch_all_metrics", metrics), \
             patch.object(a_stats, "fetch_outlier_data", AsyncMock(return_value=[])), \
             patch.object(a_stats, "fetch_user_event_counts", AsyncMock(return_value=[])):
            result = await _call_stats()
        assert metrics.await_args.kwargs["segments"] == ["platform", "user_tenure"]
        assert "backend" not in result and "unsupported_sections" not in result
        assert result["segmentation"] == {}


# ──────────────────────────────────────────────
#  Паритет с настоящим Postgres (только при SYNAQ_TEST_PG_DSN)
# ──────────────────────────────────────────────

PG_DSN = os.environ.get("SYNAQ_TEST_PG_DSN", "")

_DDL = [
    "CREATE SCHEMA events",
    """CREATE TABLE events.tracking_events (
        event_date      date        NOT NULL,
        ts              timestamptz NOT NULL,
        event_id        text        NOT NULL,
        user_id         text        NOT NULL,
        event_name      text        NOT NULL,
        experiment_name text        NOT NULL DEFAULT '',
        variant         text        NOT NULL DEFAULT '',
        event_data      jsonb,
        CONSTRAINT tracking_events_pk PRIMARY KEY (event_id, user_id, event_name, event_date),
        CONSTRAINT tracking_events_day_chk CHECK (event_date = (ts AT TIME ZONE 'UTC')::date)
    ) PARTITION BY RANGE (event_date)""",
    """CREATE INDEX tracking_events_exp_idx ON events.tracking_events
        (experiment_name, variant, event_name, user_id) WHERE experiment_name <> ''""",
    """CREATE TABLE events.tracking_events_p20260115 PARTITION OF events.tracking_events
        FOR VALUES FROM ('2026-01-15') TO ('2026-01-16')""",
    """CREATE TABLE events.tracking_events_p20260116 PARTITION OF events.tracking_events
        FOR VALUES FROM ('2026-01-16') TO ('2026-01-17')""",
]

_INSERT = """
    INSERT INTO events.tracking_events
        (event_date, ts, event_id, user_id, event_name, experiment_name, variant, event_data)
    SELECT (t.ts AT TIME ZONE 'UTC')::date, t.ts, t.event_id, t.user_id, t.event_name,
           t.experiment_name, t.variant, t.event_data::jsonb
    FROM unnest($1::timestamptz[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[])
         AS t(ts, event_id, user_id, event_name, experiment_name, variant, event_data)
    ON CONFLICT DO NOTHING
"""

EXP = "parity_exp.v1"


def _ts(day, minute):
    return datetime(2026, 1, day, 10, minute, 0, 123000, tzinfo=timezone.utc)


# (ts, event_id, user_id, event_name, experiment, variant, event_data как текст JSON или None)
_ROWS = [
    (_ts(15, 0), "e1", "TestUser_1", "page_view", EXP, "A", '{"platform": "web"}'),
    (_ts(15, 1), "e2", "TestUser_1", "purchase", EXP, "A", '{"revenue": 10}'),           # число
    (_ts(15, 2), "e3", "TestUser_2", "page_view", EXP, "A", None),
    (_ts(15, 3), "e4", "TestUser_2", "purchase", EXP, "A", '{"revenue": " 12.5 "}'),    # строка-число
    (_ts(15, 4), "e5", "TestUser_3", "purchase", EXP, "A", '{"revenue": "abc"}'),       # плохая строка
    (_ts(15, 5), "e6", "TestUser_4", "purchase", EXP, "A", "{}"),                       # нет поля
    (_ts(15, 6), "e7", "TestUser_5", "purchase", EXP, "A", '"plain string"'),           # не объект
    (_ts(15, 7), "e8", "TestUser_6", "purchase", EXP, "A", None),                       # NULL
    (_ts(15, 8), "e9", "TestUser_7", "purchase", EXP, "B", '{"revenue": 1e20}'),        # абсурдное → 0
    (_ts(15, 9), "e10", "TestUser_8", "purchase", EXP, "B", '{"revenue": "1e5"}'),      # строка с порядком
    (_ts(15, 10), "e11", "TestUser_8", "purchase", EXP, "B", '{"revenue": "1e1000"}'),  # порядок > 3 цифр → 0
    (_ts(15, 11), "e12", "TestUser_9", "page_view", EXP, "B", "[1, 2]"),
    (_ts(15, 12), "e13", "TestUser_9", "error_occurred", EXP, "B", None),
    (_ts(15, 13), "e14", "TestUser_9", "error_occurred", EXP, "B", None),
    (_ts(16, 0), "e15", "TestUser_10", "cta_click", EXP, "B", None),
    (_ts(15, 9), "e9", "TestUser_7", "purchase", EXP, "B", '{"revenue": 999}'),        # дубль e9 → отброшен
    (_ts(15, 0), "x1", "TestUser_11", "purchase", "other_exp", "A", '{"revenue": 5}'),  # чужой эксперимент
]


@pytest.mark.skipif(not PG_DSN, reason="паритет с Postgres: задайте SYNAQ_TEST_PG_DSN (одноразовая БД)")
@pytest.mark.asyncio
async def test_parity_with_real_postgres(monkeypatch):
    asyncpg = pg.asyncpg
    conn = await asyncpg.connect(PG_DSN)
    try:
        exists = await conn.fetchval("SELECT to_regnamespace('events') IS NOT NULL")
        if exists:
            pytest.skip("схема events уже существует: тест работает только с одноразовой БД")
        for ddl in _DDL:
            await conn.execute(ddl)
        await conn.execute(_INSERT, *[list(col) for col in zip(*_ROWS)])

        pool = await asyncpg.create_pool(PG_DSN, min_size=1, max_size=4)
        monkeypatch.setattr(pg, "_pool", pool)
        try:
            revenue = {r["variant"]: r for r in await pg.fetch_revenue_metrics(EXP)}
            conv = {r["variant"]: r for r in await pg.fetch_conversion_rates(EXP, "purchase")}
            base = {r["variant"]: r for r in await pg.fetch_base_metrics(EXP)}
            guard = await pg.fetch_guardrail_metrics(EXP, ["error_occurred", "payment_failed"])
            trend = await pg.fetch_daily_trend(EXP, "purchase")
            basic = await pg.fetch_basic_stats(EXP)
            all_metrics = await pg.fetch_all_metrics(EXP)
        finally:
            await pool.close()
    finally:
        if not exists:
            await conn.execute("DROP SCHEMA IF EXISTS events CASCADE")
        await conn.close()

    # Выручка: 10 + 12.5; abc / нет поля / не объект / NULL → 0
    assert revenue["A"] == {"variant": "A", "total_users": 6, "paying_users": 6,
                            "total_revenue": 22.5, "arpu": 3.75}
    # B: 1e20 → 0, "1e5" → 100000, "1e1000" → 0; дубль e9 (999) не вставлен
    assert revenue["B"]["total_users"] == 4
    assert revenue["B"]["paying_users"] == 2
    assert revenue["B"]["total_revenue"] == 100000
    assert revenue["B"]["arpu"] == 25000.0

    assert conv["A"] == {"variant": "A", "total_users": 6, "converting_users": 6,
                         "conversion_rate_pct": 100.0}
    assert conv["B"] == {"variant": "B", "total_users": 4, "converting_users": 2,
                         "conversion_rate_pct": 50.0}
    assert base["A"] == {"variant": "A", "unique_users": 6, "total_events": 8, "converting_users": 6}
    assert base["B"]["total_events"] == 7

    assert guard == [{"variant": "B", "event_name": "error_occurred",
                      "affected_users": 1, "total_occurrences": 2}]
    assert [(r["day"], r["variant"], r["daily_users"], r["daily_conversions"]) for r in trend] == [
        ("2026-01-15", "A", 6, 6), ("2026-01-15", "B", 3, 2), ("2026-01-16", "B", 1, 0),
    ]

    summary = {r["variant"]: r for r in basic["summary"]}
    assert summary["A"]["first_event"] == "2026-01-15 10:00:00.123"
    assert summary["B"]["last_event"] == "2026-01-16 10:00:00.123"
    assert basic["funnel"] == [
        {"variant": "A", "views": 2, "clicks": 0, "conversion_pct": 0.0},
        {"variant": "B", "views": 1, "clicks": 1, "conversion_pct": 100.0},
    ]
    recent = {r["user_id"]: r["event_data"] for r in basic["recent"]}
    assert recent["TestUser_5"] == "plain string"
    assert recent["TestUser_6"] == ""
    assert basic["recent"][0]["timestamp"] == "2026-01-16 10:00:00.123"
    assert len(basic["recent"]) == 15

    assert not any(isinstance(v, dict) for v in all_metrics.values())
    assert all_metrics["conversion_rates"] == list(conv.values())
