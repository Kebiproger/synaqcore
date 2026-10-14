"""Тесты OIDC-аутентификации analytics_api (stats router + /health).

analytics_api и experiment_api оба используют пакет верхнего уровня `src`,
поэтому модули analytics_api загружаются изолированно: на время импорта все
`src.*` из sys.modules убираются, в sys.path первым ставится apps/analytics_api,
после импорта прежнее состояние sys.modules / sys.path восстанавливается.
Загруженные модули analytics_api дальше используются как обычные объекты.

ClickHouse не вызывается: fetch_* в модуле stats подменяются AsyncMock.
Все ключи, токены и данные синтетические.
"""

import asyncio
import importlib
import sys
from pathlib import Path
from unittest.mock import AsyncMock, patch

import pytest
from fastapi.testclient import TestClient

from tests._jwt_utils import (
    GENERIC_ROLES_CLAIM,
    TEST_AUDIENCE,
    TEST_ISSUER,
    TEST_JWKS_URL,
    FakeClock,
    bearer,
    jwks,
    make_token,
    public_jwk,
    rsa_key,
)
ANALYTICS_API_DIR = str(Path(__file__).resolve().parent.parent / "apps" / "analytics_api")


def _is_src(name: str) -> bool:
    return name == "src" or name.startswith("src.")


def _load_analytics_modules():
    saved_modules = {k: v for k, v in sys.modules.items() if _is_src(k)}
    saved_path = list(sys.path)
    for k in saved_modules:
        del sys.modules[k]
    sys.path.insert(0, ANALYTICS_API_DIR)
    importlib.invalidate_caches()
    try:
        main = importlib.import_module("src.main")
        stats = importlib.import_module("src.api.v1.stats")
        auth = importlib.import_module("src.core.auth")
    finally:
        for k in [k for k in sys.modules if _is_src(k)]:
            del sys.modules[k]
        sys.modules.update(saved_modules)
        sys.path[:] = saved_path
        importlib.invalidate_caches()
    return main, stats, auth


a_main, a_stats, a_auth = _load_analytics_modules()

STATS_BASIC = "/api/v1/stats/test_exp_1/basic"
STATS_FULL = "/api/v1/stats/test_exp_1"


def test_loaded_modules_are_from_analytics_api():
    assert a_auth.__file__.startswith(ANALYTICS_API_DIR)
    assert a_stats.__file__.startswith(ANALYTICS_API_DIR)
    # experiment_api копия src.* не затронута
    from src.core import auth as exp_auth
    assert exp_auth is not a_auth
    assert "experiment_api" in exp_auth.__file__


@pytest.fixture(autouse=True)
def _reset_analytics_auth():
    a_auth.reset_auth_caches()
    yield
    a_auth.reset_auth_caches()


@pytest.fixture
def clock(monkeypatch):
    fake = FakeClock()
    monkeypatch.setattr(a_auth, "time", fake)
    return fake


@pytest.fixture
def fetch(monkeypatch, clock):
    monkeypatch.setattr(a_auth.settings, "OIDC_ISSUER", TEST_ISSUER)
    monkeypatch.setattr(a_auth.settings, "OIDC_AUDIENCE", TEST_AUDIENCE)
    monkeypatch.setattr(a_auth.settings, "OIDC_JWKS_URL", TEST_JWKS_URL)
    monkeypatch.setattr(a_auth.settings, "OIDC_CONFIG_FILE", "")
    mock = AsyncMock(return_value=jwks(public_jwk(rsa_key())))
    monkeypatch.setattr(a_auth, "fetch_jwks", mock)
    return mock


@pytest.fixture
def clickhouse():
    """Подмена всех обращений stats-роутера к ClickHouse."""
    basic = AsyncMock(return_value={"summary": [], "event_types": []})
    full = AsyncMock(return_value={"conversion_rates": []})
    with patch.object(a_stats, "fetch_basic_stats", basic), \
         patch.object(a_stats, "fetch_all_metrics", full), \
         patch.object(a_stats, "fetch_outlier_data", AsyncMock(return_value=[])), \
         patch.object(a_stats, "fetch_user_event_counts", AsyncMock(return_value=[])):
        yield basic, full


@pytest.fixture
def http():
    return TestClient(a_main.app, raise_server_exceptions=False)


def _assert_401(resp):
    assert resp.status_code == 401, resp.text
    assert resp.json() == {"detail": "Требуется аутентификация"}
    assert resp.headers.get("www-authenticate") == "Bearer"


class TestAnalyticsAuth:

    def test_health_is_public(self, http):
        resp = http.get("/health")
        assert resp.status_code == 200
        assert resp.json() == {"status": "OK", "service": "analytics_api"}

    @pytest.mark.parametrize("path", [STATS_BASIC, STATS_FULL])
    def test_no_token_401(self, http, fetch, clickhouse, path):
        _assert_401(http.get(path))
        clickhouse[0].assert_not_called()
        clickhouse[1].assert_not_called()

    def test_no_token_401_when_unconfigured(self, http, clickhouse):
        _assert_401(http.get(STATS_BASIC))

    @pytest.mark.parametrize("role", ["analyst", "admin"])
    def test_allowed_roles_reach_basic_stats(self, http, fetch, clickhouse, role):
        resp = http.get(STATS_BASIC, headers=bearer(make_token([role])))
        assert resp.status_code == 200, resp.text
        assert resp.json() == {"summary": [], "event_types": []}
        clickhouse[0].assert_awaited_once_with("test_exp_1")

    def test_analyst_reaches_full_analysis(self, http, fetch, clickhouse):
        resp = http.get(STATS_FULL, headers=bearer(make_token(["analyst"])))
        # Нет данных → 404 уже из бизнес-логики, т.е. аутентификация пройдена
        assert resp.status_code == 404
        clickhouse[1].assert_awaited_once()

    @pytest.mark.parametrize("roles", [["viewer"], [], None])
    @pytest.mark.parametrize("path", [STATS_BASIC, STATS_FULL])
    def test_wrong_role_403(self, http, fetch, clickhouse, roles, path):
        resp = http.get(path, headers=bearer(make_token(roles)))
        assert resp.status_code == 403
        assert resp.json() == {"detail": "Недостаточно прав"}
        clickhouse[0].assert_not_called()
        clickhouse[1].assert_not_called()

    def test_project_specific_roles_claim(self, http, fetch, clickhouse):
        token = make_token(**{f"urn:zitadel:iam:org:project:{TEST_AUDIENCE}:roles": {"analyst": {}}})
        assert http.get(STATS_BASIC, headers=bearer(token)).status_code == 200

    def test_expired_token_401(self, http, fetch, clickhouse):
        _assert_401(http.get(STATS_BASIC, headers=bearer(make_token(["analyst"], lifetime=-120))))

    def test_foreign_key_401(self, http, fetch, clickhouse):
        _assert_401(http.get(STATS_BASIC, headers=bearer(make_token(["analyst"], key=rsa_key("attacker")))))

    def test_wrong_audience_401(self, http, fetch, clickhouse):
        _assert_401(http.get(STATS_BASIC, headers=bearer(make_token(["analyst"], audience="999999999999999999"))))

    def test_unconfigured_503(self, http, fetch, clickhouse, monkeypatch):
        monkeypatch.setattr(a_auth.settings, "OIDC_JWKS_URL", "")
        resp = http.get(STATS_BASIC, headers=bearer(make_token(["analyst"])))
        assert resp.status_code == 503
        assert resp.json() == {"detail": "Аутентификация не настроена"}

    def test_jwks_unavailable_503(self, http, fetch, clickhouse):
        fetch.side_effect = ConnectionError("jwks down")
        resp = http.get(STATS_BASIC, headers=bearer(make_token(["analyst"])))
        assert resp.status_code == 503
        assert resp.json() == {"detail": "Сервис аутентификации недоступен"}

    def test_generic_roles_claim_forbidden(self, http, fetch, clickhouse):
        token = make_token(**{GENERIC_ROLES_CLAIM: {"analyst": {}}})
        assert http.get(STATS_BASIC, headers=bearer(token)).status_code == 403
        clickhouse[0].assert_not_called()

    def test_roles_list_shape(self, http, fetch, clickhouse):
        token = make_token(**{f"urn:zitadel:iam:org:project:{TEST_AUDIENCE}:roles": [{"analyst": {}}]})
        assert http.get(STATS_BASIC, headers=bearer(token)).status_code == 200

    def test_token_without_jti_401(self, http, fetch, clickhouse):
        token = make_token(["analyst"], drop=("jti",), nonce="test_nonce_1")
        _assert_401(http.get(STATS_BASIC, headers=bearer(token)))

    def test_garbage_token_unconfigured_401(self, http, clickhouse):
        _assert_401(http.get(STATS_BASIC, headers={"Authorization": "Bearer a.b.c"}))

    def test_stale_keys_after_max_age_503(self, http, fetch, clickhouse, clock):
        assert http.get(STATS_BASIC, headers=bearer(make_token(["analyst"]))).status_code == 200
        fetch.side_effect = ConnectionError("jwks down")
        clock.advance(a_auth.JWKS_MAX_STALE_SECONDS + 1)
        resp = http.get(STATS_BASIC, headers=bearer(make_token(["analyst"])))
        assert resp.status_code == 503

    @pytest.mark.asyncio
    async def test_concurrent_cold_start_single_fetch(self, fetch):
        async def slow_fetch():
            await asyncio.sleep(0.05)
            return jwks(public_jwk(rsa_key()))

        fetch.side_effect = slow_fetch
        principals = await asyncio.gather(*(a_auth.verify_token(make_token(["analyst"])) for _ in range(5)))
        assert all(p.roles == {"analyst"} for p in principals)
        fetch.assert_awaited_once()


class TestAnalyticsExperimentNameRule:
    """Имена экспериментов в analytics совпадают с правилом experiment_api (с точкой)."""

    def test_dotted_name_passes_validation(self, http, fetch, clickhouse):
        resp = http.get("/api/v1/stats/test.exp_1", headers=bearer(make_token(["analyst"])))
        # 404 «нет данных» из бизнес-логики, а не 400 от валидации имени
        assert resp.status_code == 404, resp.text
        assert clickhouse[1].await_args.kwargs["experiment_name"] == "test.exp_1"

    @pytest.mark.parametrize("name", ["bad'name", "bad%20name", "a" * 51])
    def test_unsafe_name_400(self, http, fetch, clickhouse, name):
        resp = http.get(f"/api/v1/stats/{name}", headers=bearer(make_token(["analyst"])))
        assert resp.status_code == 400, resp.text
        clickhouse[1].assert_not_called()
