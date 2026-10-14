"""Тесты OIDC-аутентификации (Zitadel JWT + JWKS) в experiment_api и PATCH эксперимента.

Проверяем:
  - роли admin / analyst на Admin API (201 / 200 / 403); роли только из проектного claim
  - jti обязателен (ID token как bearer → 401)
  - 401 + WWW-Authenticate для отсутствующего / невалидного токена
  - exp, nbf, iss, aud (строка и список), обязательные claims, alg=none, HS256-confusion
  - неизвестный kid → перезапрос JWKS (не чаще раза в 30 c), ротация ключей, устаревшие ключи
  - 503 при отсутствии настроек, недоступном JWKS и ключах старше 24 ч
  - холодный старт: параллельные запросы делят один запрос JWKS
  - fallback issuer/audience из OIDC_CONFIG_FILE
  - PATCH /experiments/{name} (409 при смене вариантов активного эксперимента)
  - сброс кэша Redis, если запись не удалась

JWKS не скачивается: src.core.auth.fetch_jwks подменяется AsyncMock.
Все ключи, токены и данные синтетические.
"""

import asyncio
import json
from types import SimpleNamespace
from unittest.mock import AsyncMock, MagicMock, patch

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from src.api.v1 import admin as admin_module
from src.core import auth
from src.core.auth import Principal, get_current_principal
from src.core.database import get_session
from src.schemas.experiment import ExperimentConfigCreate
from src.services.repository import ActiveVariantsChangeError, ExperimentRepository

from tests._jwt_utils import (
    GENERIC_ROLES_CLAIM,
    ROLES_CLAIM,
    TEST_AUDIENCE,
    TEST_ISSUER,
    TEST_JWKS_URL,
    TEST_KID,
    FakeClock,
    bearer,
    claims,
    hs256_with_public_pem,
    jwks,
    make_token,
    none_alg_token,
    public_jwk,
    rsa_key,
)

EXPERIMENTS = "/api/v1/experiments/"
UNAUTH_DETAIL = "Требуется аутентификация"


# ────────────────────────────────
# Фикстуры
# ────────────────────────────────

async def _fake_session():
    yield MagicMock(name="FakeSession")


def _build_app() -> FastAPI:
    app = FastAPI()
    app.include_router(admin_module.router, prefix="/api/v1")
    app.dependency_overrides[get_session] = _fake_session
    return app


@pytest.fixture
def http():
    return TestClient(_build_app(), raise_server_exceptions=False)


@pytest.fixture
def clock(monkeypatch):
    fake = FakeClock()
    monkeypatch.setattr(auth, "time", fake)
    return fake


@pytest.fixture
def fetch(monkeypatch, clock):
    """OIDC настроен, JWKS отдаёт один ключ TEST_KID. Возвращает мок fetch_jwks."""
    monkeypatch.setattr(auth.settings, "OIDC_ISSUER", TEST_ISSUER)
    monkeypatch.setattr(auth.settings, "OIDC_AUDIENCE", TEST_AUDIENCE)
    monkeypatch.setattr(auth.settings, "OIDC_JWKS_URL", TEST_JWKS_URL)
    monkeypatch.setattr(auth.settings, "OIDC_CONFIG_FILE", "")
    mock = AsyncMock(return_value=jwks(public_jwk(rsa_key())))
    monkeypatch.setattr(auth, "fetch_jwks", mock)
    return mock


def _repo_mock(update_return=None, update_side_effect=None):
    instance = MagicMock(name="RepoInstance")
    instance.list_all = AsyncMock(return_value=[
        ExperimentConfigCreate(name="test_exp_1", variants={"A": 1, "B": 3}, url="/p1"),
    ])
    instance.exists = AsyncMock(return_value=False)
    instance.create = AsyncMock(return_value=None)
    instance.delete = AsyncMock(return_value=True)
    instance.update = AsyncMock(return_value=update_return, side_effect=update_side_effect)
    return MagicMock(name="RepoClass", return_value=instance), instance


def _redis_mock(save_side_effect=None):
    cache = MagicMock(name="RedisCache")
    cache.save_experiment = AsyncMock(side_effect=save_side_effect)
    cache.delete_experiment = AsyncMock()
    return cache


@pytest.fixture
def backend():
    """Подменённые репозиторий и Redis для admin-роутера."""
    repo_cls, repo = _repo_mock()
    cache = _redis_mock()
    with patch.object(admin_module, "ExperimentRepository", repo_cls), \
         patch.object(admin_module, "redis_cache", cache):
        yield repo, cache


def _assert_401(resp):
    assert resp.status_code == 401, resp.text
    assert resp.json() == {"detail": UNAUTH_DETAIL}
    assert resp.headers.get("www-authenticate") == "Bearer"


NEW_EXP = {"name": "test_exp_new", "variants": {"A": 50, "B": 50}}


# ────────────────────────────────
# 1. Роли
# ────────────────────────────────

class TestRoles:

    def test_admin_can_create(self, http, fetch, backend):
        repo, cache = backend
        resp = http.post(EXPERIMENTS, json=NEW_EXP, headers=bearer(make_token(["admin"])))
        assert resp.status_code == 201, resp.text
        assert resp.json()["name"] == "test_exp_new"
        repo.create.assert_awaited_once()
        fetch.assert_awaited_once()

    def test_admin_can_list(self, http, fetch, backend):
        resp = http.get(EXPERIMENTS, headers=bearer(make_token(["admin"])))
        assert resp.status_code == 200

    def test_analyst_can_list(self, http, fetch, backend):
        resp = http.get(EXPERIMENTS, headers=bearer(make_token(["analyst"])))
        assert resp.status_code == 200
        assert resp.json() == [{
            "name": "test_exp_1", "variants": {"A": 1, "B": 3}, "config": None,
            "url": "/p1", "is_active": True, "total_weight": 4,
        }]

    @pytest.mark.parametrize("method,path,body", [
        ("POST", EXPERIMENTS, NEW_EXP),
        ("DELETE", EXPERIMENTS + "test_exp_1", None),
        ("PATCH", EXPERIMENTS + "test_exp_1", {"is_active": False}),
    ])
    def test_analyst_forbidden_on_writes(self, http, fetch, backend, method, path, body):
        repo, _ = backend
        resp = http.request(method, path, json=body, headers=bearer(make_token(["analyst"])))
        assert resp.status_code == 403
        assert resp.json() == {"detail": "Недостаточно прав"}
        repo.create.assert_not_called()
        repo.delete.assert_not_called()
        repo.update.assert_not_called()

    @pytest.mark.parametrize("roles", [[], ["viewer"], None])
    def test_no_known_role_forbidden_on_list(self, http, fetch, backend, roles):
        resp = http.get(EXPERIMENTS, headers=bearer(make_token(roles)))
        assert resp.status_code == 403

    def test_project_specific_roles_claim(self, http, fetch, backend):
        project_claim = f"urn:zitadel:iam:org:project:{TEST_AUDIENCE}:roles"
        token = make_token(**{project_claim: {"admin": {"100000000000000002": "test.example.com"}}})
        resp = http.post(EXPERIMENTS, json=NEW_EXP, headers=bearer(token))
        assert resp.status_code == 201

    def test_other_project_roles_claim_ignored(self, http, fetch, backend):
        foreign = "urn:zitadel:iam:org:project:999999999999999999:roles"
        token = make_token(**{foreign: {"admin": {}}})
        resp = http.get(EXPERIMENTS, headers=bearer(token))
        assert resp.status_code == 403

    def test_roles_claim_not_a_dict_ignored(self, http, fetch, backend):
        token = make_token(**{ROLES_CLAIM: ["admin"]})
        resp = http.get(EXPERIMENTS, headers=bearer(token))
        assert resp.status_code == 403

    def test_generic_roles_claim_alone_forbidden(self, http, fetch, backend):
        """Общий claim может нести роли другого проекта — не доверяем ему."""
        token = make_token(**{GENERIC_ROLES_CLAIM: {"admin": {"100000000000000002": "test.example.com"}}})
        resp = http.post(EXPERIMENTS, json=NEW_EXP, headers=bearer(token))
        assert resp.status_code == 403
        backend[0].create.assert_not_called()

    def test_roles_claim_list_of_objects(self, http, fetch, backend):
        token = make_token(**{ROLES_CLAIM: [{"admin": {"100000000000000002": "test.example.com"}}]})
        resp = http.post(EXPERIMENTS, json=NEW_EXP, headers=bearer(token))
        assert resp.status_code == 201


# ────────────────────────────────
# 2. Отсутствующий / битый токен → 401
# ────────────────────────────────

class TestMissingOrMalformed:

    def test_no_token(self, http, fetch, backend):
        _assert_401(http.get(EXPERIMENTS))
        fetch.assert_not_called()

    def test_no_token_when_unconfigured_still_401(self, http, backend):
        # OIDC_* пустые (conftest) → но без токена ответ 401, а не 503
        _assert_401(http.post(EXPERIMENTS, json=NEW_EXP))

    @pytest.mark.parametrize("header", [
        "Basic dGVzdF91c2VyOnRlc3RfcGFzcw==",
        "Bearer",
        "Bearer ",
        "Bearer not-a-jwt",
        "Bearer a.b.c",
        "Token abc",
    ])
    def test_malformed_authorization(self, http, fetch, backend, header):
        _assert_401(http.get(EXPERIMENTS, headers={"Authorization": header}))

    @pytest.mark.parametrize("header", ["Bearer not-a-jwt", "Bearer a.b.c"])
    def test_garbage_token_when_unconfigured_401(self, http, backend, header):
        # OIDC не настроен (conftest), но мусор вместо токена — 401, а не 503
        _assert_401(http.get(EXPERIMENTS, headers={"Authorization": header}))

    def test_missing_kid_when_unconfigured_401(self, http, backend):
        _assert_401(http.get(EXPERIMENTS, headers=bearer(make_token(["admin"], kid=None))))

    def test_missing_kid(self, http, fetch, backend):
        _assert_401(http.get(EXPERIMENTS, headers=bearer(make_token(["admin"], kid=None))))
        fetch.assert_not_called()

    def test_too_long_token(self, http, fetch, backend):
        token = make_token(["admin"], padding="x" * 9000)
        assert len(token) > 8192
        _assert_401(http.get(EXPERIMENTS, headers=bearer(token)))
        fetch.assert_not_called()

    def test_error_response_does_not_echo_token(self, http, fetch, backend):
        token = make_token(["admin"], issuer="https://evil.example.com")
        resp = http.get(EXPERIMENTS, headers=bearer(token))
        _assert_401(resp)
        for part in token.split("."):
            assert part not in resp.text
            assert all(part not in v for v in resp.headers.values())
        assert "evil.example.com" not in resp.text


# ────────────────────────────────
# 3. Claims и подпись → 401
# ────────────────────────────────

class TestClaims:

    def test_expired(self, http, fetch, backend):
        _assert_401(http.get(EXPERIMENTS, headers=bearer(make_token(["admin"], lifetime=-120))))

    def test_expired_within_leeway_accepted(self, http, fetch, backend):
        resp = http.get(EXPERIMENTS, headers=bearer(make_token(["admin"], lifetime=-10)))
        assert resp.status_code == 200

    def test_nbf_in_future(self, http, fetch, backend):
        import time
        token = make_token(["admin"], nbf=int(time.time()) + 600, lifetime=3600)
        _assert_401(http.get(EXPERIMENTS, headers=bearer(token)))

    def test_wrong_issuer(self, http, fetch, backend):
        _assert_401(http.get(EXPERIMENTS, headers=bearer(make_token(["admin"], issuer="https://other.example.com"))))

    def test_wrong_audience(self, http, fetch, backend):
        _assert_401(http.get(EXPERIMENTS, headers=bearer(make_token(["admin"], audience="999999999999999999"))))

    def test_audience_list_containing_project(self, http, fetch, backend):
        token = make_token(["admin"], audience=["test_client_1@example", TEST_AUDIENCE])
        assert http.get(EXPERIMENTS, headers=bearer(token)).status_code == 200

    def test_audience_list_without_project(self, http, fetch, backend):
        token = make_token(["admin"], audience=["test_client_1@example", "999999999999999999"])
        _assert_401(http.get(EXPERIMENTS, headers=bearer(token)))

    @pytest.mark.parametrize("claim", ["exp", "iat", "iss", "aud", "sub", "jti"])
    def test_required_claim_missing(self, http, fetch, backend, claim):
        _assert_401(http.get(EXPERIMENTS, headers=bearer(make_token(["admin"], drop=(claim,)))))

    def test_id_token_as_bearer_rejected(self, http, fetch, backend):
        """ID token Zitadel не содержит jti (зато nonce / at_hash) — как bearer не принимается."""
        token = make_token(["admin"], drop=("jti",), nonce="test_nonce_1", at_hash="test_at_hash_1",
                           auth_time=1700000000)
        _assert_401(http.post(EXPERIMENTS, json=NEW_EXP, headers=bearer(token)))
        backend[0].create.assert_not_called()

    def test_empty_sub(self, http, fetch, backend):
        _assert_401(http.get(EXPERIMENTS, headers=bearer(make_token(["admin"], sub=""))))

    def test_alg_none(self, http, fetch, backend):
        _assert_401(http.get(EXPERIMENTS, headers=bearer(none_alg_token(["admin"]))))

    @pytest.mark.parametrize("header_alg", ["HS256", "RS256"])
    def test_hmac_with_public_key_confusion(self, http, fetch, backend, header_alg):
        token = hs256_with_public_pem(["admin"], header_alg=header_alg)
        _assert_401(http.post(EXPERIMENTS, json=NEW_EXP, headers=bearer(token)))
        backend[0].create.assert_not_called()

    def test_signed_by_other_key_same_kid(self, http, fetch, backend):
        token = make_token(["admin"], key=rsa_key("attacker"))
        _assert_401(http.post(EXPERIMENTS, json=NEW_EXP, headers=bearer(token)))
        backend[0].create.assert_not_called()

    def test_tampered_payload(self, http, fetch, backend):
        good = make_token(["analyst"])
        forged = make_token(["admin"], key=rsa_key("attacker"))
        h, _, s = good.split(".")
        token = f"{h}.{forged.split('.')[1]}.{s}"
        _assert_401(http.post(EXPERIMENTS, json=NEW_EXP, headers=bearer(token)))


# ────────────────────────────────
# 4. JWKS: kid, перезапрос, rate limit, устаревшие ключи
# ────────────────────────────────

class TestJwks:

    def test_keys_cached_between_requests(self, http, fetch, backend):
        for _ in range(3):
            assert http.get(EXPERIMENTS, headers=bearer(make_token(["admin"]))).status_code == 200
        fetch.assert_awaited_once()

    def test_unknown_kid_triggers_refetch_then_401(self, http, fetch, backend, clock):
        assert http.get(EXPERIMENTS, headers=bearer(make_token(["admin"]))).status_code == 200
        assert fetch.await_count == 1
        clock.advance(31)
        _assert_401(http.get(EXPERIMENTS, headers=bearer(make_token(["admin"], kid="test_kid_unknown"))))
        assert fetch.await_count == 2

    def test_unknown_kid_refetch_rate_limited(self, http, fetch, backend, clock):
        assert http.get(EXPERIMENTS, headers=bearer(make_token(["admin"]))).status_code == 200
        clock.advance(31)
        unknown = bearer(make_token(["admin"], kid="test_kid_unknown"))
        _assert_401(http.get(EXPERIMENTS, headers=unknown))
        clock.advance(10)
        _assert_401(http.get(EXPERIMENTS, headers=unknown))
        _assert_401(http.get(EXPERIMENTS, headers=bearer(make_token(["admin"], kid="test_kid_unknown_2"))))
        assert fetch.await_count == 2  # первичная загрузка + один перезапрос
        clock.advance(25)  # 35 c с последней попытки
        _assert_401(http.get(EXPERIMENTS, headers=unknown))
        assert fetch.await_count == 3

    def test_unknown_kid_first_request_fetches_once(self, http, fetch, backend):
        unknown = bearer(make_token(["admin"], kid="test_kid_unknown"))
        _assert_401(http.get(EXPERIMENTS, headers=unknown))
        _assert_401(http.get(EXPERIMENTS, headers=unknown))
        fetch.assert_awaited_once()

    def test_key_rotation_new_kid_picked_up(self, http, fetch, backend, clock):
        assert http.get(EXPERIMENTS, headers=bearer(make_token(["admin"]))).status_code == 200
        fetch.return_value = jwks(
            public_jwk(rsa_key()),
            public_jwk(rsa_key("rotated"), kid="test_kid_2"),
        )
        clock.advance(31)
        token = make_token(["admin"], key=rsa_key("rotated"), kid="test_kid_2")
        assert http.get(EXPERIMENTS, headers=bearer(token)).status_code == 200
        assert fetch.await_count == 2

    def test_stale_keys_used_when_refetch_fails(self, http, fetch, backend, clock):
        assert http.get(EXPERIMENTS, headers=bearer(make_token(["admin"]))).status_code == 200
        fetch.side_effect = ConnectionError("jwks down")
        clock.advance(auth.JWKS_TTL_SECONDS + 1)  # ключи протухли → перезапрос, он падает
        assert http.get(EXPERIMENTS, headers=bearer(make_token(["admin"]))).status_code == 200
        assert fetch.await_count == 2

    def test_stale_keys_kept_when_refetch_returns_empty(self, http, fetch, backend, clock):
        assert http.get(EXPERIMENTS, headers=bearer(make_token(["admin"]))).status_code == 200
        fetch.return_value = {"keys": []}
        clock.advance(auth.JWKS_TTL_SECONDS + 1)
        assert http.get(EXPERIMENTS, headers=bearer(make_token(["admin"]))).status_code == 200

    def test_stale_keys_rejected_after_max_age(self, http, fetch, backend, clock):
        assert http.get(EXPERIMENTS, headers=bearer(make_token(["admin"]))).status_code == 200
        fetch.side_effect = ConnectionError("jwks down")
        clock.advance(auth.JWKS_MAX_STALE_SECONDS - 60)
        assert http.get(EXPERIMENTS, headers=bearer(make_token(["admin"]))).status_code == 200
        clock.advance(120)  # теперь ключам больше суток
        resp = http.get(EXPERIMENTS, headers=bearer(make_token(["admin"])))
        assert resp.status_code == 503
        assert resp.json() == {"detail": "Сервис аутентификации недоступен"}
        # JWKS снова доступен → после rate limit ключи обновляются
        fetch.side_effect = None
        clock.advance(auth.JWKS_MIN_REFRESH_INTERVAL + 1)
        assert http.get(EXPERIMENTS, headers=bearer(make_token(["admin"]))).status_code == 200

    @pytest.mark.parametrize("bad_jwk_extra", [{"use": "enc"}, {"alg": "RS512"}, {"kty": "EC"}, {"kid": ""}])
    def test_unusable_jwk_ignored(self, http, fetch, backend, bad_jwk_extra):
        fetch.return_value = jwks(public_jwk(rsa_key(), **bad_jwk_extra))
        resp = http.get(EXPERIMENTS, headers=bearer(make_token(["admin"])))
        assert resp.status_code == 503
        assert resp.json() == {"detail": "Сервис аутентификации недоступен"}

    @pytest.mark.parametrize("extra", [{"use": "sig"}, {"alg": "RS256"}, {"use": "sig", "alg": "RS256"}])
    def test_jwk_with_sig_use_and_rs256_alg_accepted(self, http, fetch, backend, extra):
        fetch.return_value = jwks(public_jwk(rsa_key(), **extra))
        assert http.get(EXPERIMENTS, headers=bearer(make_token(["admin"]))).status_code == 200


# ────────────────────────────────
# 5. 503: не настроено / JWKS недоступен
# ────────────────────────────────

class TestUnavailable:

    @pytest.mark.parametrize("missing", ["OIDC_ISSUER", "OIDC_AUDIENCE", "OIDC_JWKS_URL"])
    def test_unconfigured_returns_503(self, http, fetch, backend, monkeypatch, missing):
        monkeypatch.setattr(auth.settings, missing, "")
        resp = http.get(EXPERIMENTS, headers=bearer(make_token(["admin"])))
        assert resp.status_code == 503
        assert resp.json() == {"detail": "Аутентификация не настроена"}
        fetch.assert_not_called()

    def test_jwks_fetch_fails_returns_503(self, http, fetch, backend):
        fetch.side_effect = ConnectionError("jwks down")
        resp = http.get(EXPERIMENTS, headers=bearer(make_token(["admin"])))
        assert resp.status_code == 503
        assert resp.json() == {"detail": "Сервис аутентификации недоступен"}

    def test_jwks_garbage_returns_503(self, http, fetch, backend):
        fetch.return_value = ["not", "a", "jwks"]
        resp = http.get(EXPERIMENTS, headers=bearer(make_token(["admin"])))
        assert resp.status_code == 503

    def test_jwks_recovers_after_empty_retry_interval(self, http, fetch, backend, clock):
        fetch.side_effect = ConnectionError("jwks down")
        assert http.get(EXPERIMENTS, headers=bearer(make_token(["admin"]))).status_code == 503
        fetch.side_effect = None
        clock.advance(auth.JWKS_EMPTY_RETRY_INTERVAL + 1)
        assert http.get(EXPERIMENTS, headers=bearer(make_token(["admin"]))).status_code == 200


# ────────────────────────────────
# 6. OIDC_CONFIG_FILE fallback
# ────────────────────────────────

class TestConfigFile:

    def _write(self, tmp_path, data) -> str:
        path = tmp_path / "oidc.json"
        path.write_text(json.dumps(data), encoding="utf-8")
        return str(path)

    @pytest.fixture
    def file_config(self, fetch, monkeypatch, tmp_path):
        monkeypatch.setattr(auth.settings, "OIDC_ISSUER", "")
        monkeypatch.setattr(auth.settings, "OIDC_AUDIENCE", "")
        path = self._write(tmp_path, {
            "issuer": TEST_ISSUER, "project_id": TEST_AUDIENCE, "client_id": "test_client_1",
        })
        monkeypatch.setattr(auth.settings, "OIDC_CONFIG_FILE", path)
        return path

    def test_issuer_and_audience_from_file(self, http, file_config, backend):
        assert http.get(EXPERIMENTS, headers=bearer(make_token(["admin"]))).status_code == 200

    def test_project_roles_claim_uses_file_project_id(self, http, file_config, backend):
        token = make_token(**{f"urn:zitadel:iam:org:project:{TEST_AUDIENCE}:roles": {"analyst": {}}})
        assert http.get(EXPERIMENTS, headers=bearer(token)).status_code == 200

    def test_env_audience_overrides_file(self, http, file_config, backend, monkeypatch):
        monkeypatch.setattr(auth.settings, "OIDC_AUDIENCE", "200000000000000002")
        _assert_401(http.get(EXPERIMENTS, headers=bearer(make_token(["admin"]))))
        token = make_token(["admin"], audience="200000000000000002")
        assert http.get(EXPERIMENTS, headers=bearer(token)).status_code == 200

    def test_env_issuer_overrides_file(self, http, file_config, backend, monkeypatch):
        monkeypatch.setattr(auth.settings, "OIDC_ISSUER", "https://other.example.com")
        _assert_401(http.get(EXPERIMENTS, headers=bearer(make_token(["admin"]))))

    def test_missing_file_returns_503(self, http, fetch, backend, monkeypatch, tmp_path):
        monkeypatch.setattr(auth.settings, "OIDC_AUDIENCE", "")
        monkeypatch.setattr(auth.settings, "OIDC_CONFIG_FILE", str(tmp_path / "absent.json"))
        resp = http.get(EXPERIMENTS, headers=bearer(make_token(["admin"])))
        assert resp.status_code == 503
        assert resp.json() == {"detail": "Аутентификация не настроена"}

    @pytest.mark.parametrize("content", ['{"issuer": "https://auth.example.com"}', "not json", "[1, 2]",
                                         '{"project_id": 123}'])
    def test_invalid_file_returns_503(self, http, fetch, backend, monkeypatch, tmp_path, content):
        monkeypatch.setattr(auth.settings, "OIDC_AUDIENCE", "")
        path = tmp_path / "oidc.json"
        path.write_text(content, encoding="utf-8")
        monkeypatch.setattr(auth.settings, "OIDC_CONFIG_FILE", str(path))
        assert http.get(EXPERIMENTS, headers=bearer(make_token(["admin"]))).status_code == 503

    def test_file_appearing_later_is_picked_up(self, http, fetch, backend, monkeypatch, tmp_path):
        monkeypatch.setattr(auth.settings, "OIDC_AUDIENCE", "")
        path = tmp_path / "oidc.json"
        monkeypatch.setattr(auth.settings, "OIDC_CONFIG_FILE", str(path))
        assert http.get(EXPERIMENTS, headers=bearer(make_token(["admin"]))).status_code == 503
        path.write_text(json.dumps({"issuer": TEST_ISSUER, "project_id": TEST_AUDIENCE}), encoding="utf-8")
        assert http.get(EXPERIMENTS, headers=bearer(make_token(["admin"]))).status_code == 200


# ────────────────────────────────
# 7. verify_token / require_roles напрямую
# ────────────────────────────────

class TestVerifyTokenUnit:

    @pytest.mark.asyncio
    async def test_returns_principal(self, fetch):
        principal = await auth.verify_token(make_token(["admin", "analyst"], sub="test_user_42"))
        assert principal == Principal(sub="test_user_42", roles={"admin", "analyst"})

    @pytest.mark.asyncio
    async def test_generic_roles_ignored(self, fetch):
        token = make_token(["analyst"], **{GENERIC_ROLES_CLAIM: {"admin": {}}})
        principal = await auth.verify_token(token)
        assert principal.roles == {"analyst"}

    @pytest.mark.asyncio
    async def test_list_shape_roles_merged(self, fetch):
        token = make_token(**{ROLES_CLAIM: [{"admin": {}}, {"analyst": {}}, "junk", {"": {}}]})
        principal = await auth.verify_token(token)
        assert principal.roles == {"admin", "analyst"}

    @pytest.mark.asyncio
    async def test_concurrent_cold_start_single_fetch(self, fetch):
        """Пока идёт первая загрузка JWKS, остальные запросы ждут её, а не получают 503."""
        async def slow_fetch():
            await asyncio.sleep(0.05)
            return jwks(public_jwk(rsa_key()))

        fetch.side_effect = slow_fetch
        tokens = [make_token(["admin"], sub=f"test_user_{i}") for i in range(8)]
        principals = await asyncio.gather(*(auth.verify_token(t) for t in tokens))
        assert [p.sub for p in principals] == [f"test_user_{i}" for i in range(8)]
        assert all(p.roles == {"admin"} for p in principals)
        fetch.assert_awaited_once()

    @pytest.mark.asyncio
    async def test_concurrent_cold_start_fetch_fails_all_503(self, fetch):
        async def failing_fetch():
            await asyncio.sleep(0.02)
            raise ConnectionError("jwks down")

        fetch.side_effect = failing_fetch
        results = await asyncio.gather(
            *(auth.verify_token(make_token(["admin"])) for _ in range(5)), return_exceptions=True,
        )
        assert all(getattr(r, "status_code", None) == 503 for r in results)
        fetch.assert_awaited_once()

    @pytest.mark.asyncio
    async def test_cancelled_waiter_does_not_cancel_shared_fetch(self, fetch):
        async def slow_fetch():
            await asyncio.sleep(0.05)
            return jwks(public_jwk(rsa_key()))

        fetch.side_effect = slow_fetch
        first = asyncio.ensure_future(auth.verify_token(make_token(["admin"])))
        second = asyncio.ensure_future(auth.verify_token(make_token(["admin"], sub="test_user_2")))
        await asyncio.sleep(0.01)
        first.cancel()
        principal = await second
        assert principal.sub == "test_user_2"
        fetch.assert_awaited_once()

    def test_require_roles_without_roles_rejected(self):
        with pytest.raises(ValueError):
            auth.require_roles()


# ────────────────────────────────
# 8. PATCH /experiments/{name}
# ────────────────────────────────

UPDATED = ExperimentConfigCreate(
    name="test_exp_1",
    variants={"A": 30, "B": 70},
    config={"A": {"color": "red"}},
    url="/test-page",
    is_active=False,
)


@pytest.fixture
def admin_http():
    app = _build_app()
    app.dependency_overrides[get_current_principal] = lambda: Principal(sub="test_admin", roles={"admin"})
    return TestClient(app, raise_server_exceptions=False)


def _patch_backend(update_return=UPDATED, save_side_effect=None, update_side_effect=None):
    repo_cls, repo = _repo_mock(update_return=update_return, update_side_effect=update_side_effect)
    cache = _redis_mock(save_side_effect=save_side_effect)
    return repo_cls, repo, cache


class TestPatchExperiment:

    URL = EXPERIMENTS + "test_exp_1"

    def _patch(self, client, body, **backend_kwargs):
        repo_cls, repo, cache = _patch_backend(**backend_kwargs)
        with patch.object(admin_module, "ExperimentRepository", repo_cls), \
             patch.object(admin_module, "redis_cache", cache):
            resp = client.patch(self.URL, json=body)
        return resp, repo, cache

    def test_toggle_is_active(self, admin_http):
        resp, repo, cache = self._patch(admin_http, {"is_active": False})
        assert resp.status_code == 200, resp.text
        assert resp.json() == {
            "name": "test_exp_1",
            "variants": {"A": 30, "B": 70},
            "config": {"A": {"color": "red"}},
            "url": "/test-page",
            "is_active": False,
            "total_weight": 100,
        }
        repo.update.assert_awaited_once_with("test_exp_1", is_active=False)
        cache.save_experiment.assert_awaited_once_with(UPDATED)

    def test_update_variants(self, admin_http):
        resp, repo, _ = self._patch(admin_http, {"variants": {"A": 30, "B": 70}})
        assert resp.status_code == 200
        repo.update.assert_awaited_once_with("test_exp_1", variants={"A": 30, "B": 70})

    def test_update_several_fields(self, admin_http):
        body = {"config": {"A": {"color": "red"}}, "url": "/test-page", "is_active": True}
        resp, repo, _ = self._patch(admin_http, body)
        assert resp.status_code == 200
        repo.update.assert_awaited_once_with("test_exp_1", **body)

    @pytest.mark.parametrize("field", ["config", "url"])
    def test_null_clears_optional_field(self, admin_http, field):
        resp, repo, _ = self._patch(admin_http, {field: None})
        assert resp.status_code == 200
        repo.update.assert_awaited_once_with("test_exp_1", **{field: None})

    @pytest.mark.parametrize("variants", [{"A": 0, "B": 50}, {"A": -1}, {"A": "abc"}])
    def test_invalid_weights_422(self, admin_http, variants):
        resp, repo, _ = self._patch(admin_http, {"variants": variants})
        assert resp.status_code == 422
        repo.update.assert_not_called()

    @pytest.mark.parametrize("body", [{"name": "test_exp_renamed"}, {"is_active": True, "unknown": 1}])
    def test_extra_fields_422(self, admin_http, body):
        resp, repo, _ = self._patch(admin_http, body)
        assert resp.status_code == 422
        repo.update.assert_not_called()

    @pytest.mark.parametrize("field", ["is_active", "variants"])
    def test_null_required_field_422(self, admin_http, field):
        resp, repo, _ = self._patch(admin_http, {field: None})
        assert resp.status_code == 422
        repo.update.assert_not_called()

    def test_url_too_long_422(self, admin_http):
        resp, repo, _ = self._patch(admin_http, {"url": "/" + "p" * 250})
        assert resp.status_code == 422

    def test_empty_body_400(self, admin_http):
        resp, repo, cache = self._patch(admin_http, {})
        assert resp.status_code == 400
        repo.update.assert_not_called()
        cache.save_experiment.assert_not_called()

    def test_missing_experiment_404(self, admin_http):
        resp, repo, cache = self._patch(admin_http, {"is_active": False}, update_return=None)
        assert resp.status_code == 404
        cache.save_experiment.assert_not_called()

    @pytest.mark.parametrize("exc", [ConnectionError("redis down"), RuntimeError("Redis не инициализирован")])
    def test_redis_save_fails_still_200(self, admin_http, exc):
        resp, repo, cache = self._patch(admin_http, {"is_active": False}, save_side_effect=exc)
        assert resp.status_code == 200
        assert resp.json()["is_active"] is False
        cache.save_experiment.assert_awaited_once()
        # Устаревший конфиг не должен жить в кэше до TTL
        cache.delete_experiment.assert_awaited_once_with("test_exp_1")

    def test_redis_save_ok_no_delete(self, admin_http):
        resp, _, cache = self._patch(admin_http, {"is_active": False})
        assert resp.status_code == 200
        cache.delete_experiment.assert_not_called()

    def test_redis_save_and_delete_fail_still_200(self, admin_http):
        repo_cls, repo, cache = _patch_backend(save_side_effect=ConnectionError("redis down"))
        cache.delete_experiment.side_effect = ConnectionError("redis down")
        with patch.object(admin_module, "ExperimentRepository", repo_cls), \
             patch.object(admin_module, "redis_cache", cache):
            resp = admin_http.patch(self.URL, json={"is_active": False})
        assert resp.status_code == 200

    def test_active_variants_change_409(self, admin_http):
        resp, repo, cache = self._patch(
            admin_http, {"variants": {"A": 10, "B": 90}},
            update_side_effect=ActiveVariantsChangeError("test_exp_1"),
        )
        assert resp.status_code == 409
        assert "активного эксперимента" in resp.json()["detail"]
        cache.save_experiment.assert_not_called()

    def test_patch_requires_auth(self, http):
        _assert_401(http.patch(TestPatchExperiment.URL, json={"is_active": False}))

    def test_patch_with_real_admin_token(self, http, fetch):
        repo_cls, repo, cache = _patch_backend()
        with patch.object(admin_module, "ExperimentRepository", repo_cls), \
             patch.object(admin_module, "redis_cache", cache):
            resp = http.patch(self.URL, json={"is_active": False}, headers=bearer(make_token(["admin"])))
        assert resp.status_code == 200


# ────────────────────────────────
# 9. POST: сброс кэша при ошибке записи
# ────────────────────────────────

class TestCreateCache:

    def _create(self, client, save_side_effect=None):
        repo_cls, repo = _repo_mock()
        cache = _redis_mock(save_side_effect=save_side_effect)
        with patch.object(admin_module, "ExperimentRepository", repo_cls), \
             patch.object(admin_module, "redis_cache", cache):
            resp = client.post(EXPERIMENTS, json=NEW_EXP)
        return resp, repo, cache

    def test_save_fails_deletes_key(self, admin_http):
        resp, repo, cache = self._create(admin_http, save_side_effect=ConnectionError("redis down"))
        assert resp.status_code == 201
        repo.create.assert_awaited_once()
        cache.delete_experiment.assert_awaited_once_with("test_exp_new")

    def test_save_ok_no_delete(self, admin_http):
        resp, _, cache = self._create(admin_http)
        assert resp.status_code == 201
        cache.delete_experiment.assert_not_called()

    @pytest.mark.parametrize("body", [
        {"name": "active", "variants": {"A": 1}},
        {"name": "config", "variants": {"A": 1}},
        {"name": "<img src=x>", "variants": {"A": 1}},
        {"name": "test exp", "variants": {"A": 1}},
        {"name": "test_exp_new", "variants": {}},
        {"name": "test_exp_new", "variants": {"a b": 1}},
        {"name": "test_exp_new", "variants": {f"v{i}": 1 for i in range(21)}},
    ])
    def test_invalid_body_422(self, admin_http, body):
        repo_cls, repo = _repo_mock()
        with patch.object(admin_module, "ExperimentRepository", repo_cls), \
             patch.object(admin_module, "redis_cache", _redis_mock()):
            resp = admin_http.post(EXPERIMENTS, json=body)
        assert resp.status_code == 422
        repo.create.assert_not_called()


# ────────────────────────────────
# 10. ExperimentRepository.update: блокировка смены вариантов активного эксперимента
# ────────────────────────────────

def _fake_repo_session(is_active: bool, variants=None):
    model = SimpleNamespace(
        name="test_exp_1", variants=variants or {"A": 50, "B": 50},
        config=None, url=None, is_active=is_active,
    )
    session = MagicMock(name="FakeSession")
    session.get = AsyncMock(return_value=model)
    session.commit = AsyncMock()
    session.rollback = AsyncMock()
    return session, model


class TestRepositoryVariantsLock:

    @pytest.mark.asyncio
    @pytest.mark.parametrize("is_active,fields", [
        (True, {"variants": {"A": 10, "B": 90}}),
        (True, {"variants": {"A": 10, "B": 90}, "is_active": True}),
        (False, {"variants": {"A": 10, "B": 90}, "is_active": True}),  # включение + смена
        (True, {"variants": {"A": 50, "B": 50, "C": 1}, "url": "/p"}),
    ])
    async def test_change_rejected(self, is_active, fields):
        session, model = _fake_repo_session(is_active)
        with pytest.raises(ActiveVariantsChangeError):
            await ExperimentRepository(session).update("test_exp_1", **fields)
        session.commit.assert_not_called()
        session.rollback.assert_awaited_once()
        assert model.variants == {"A": 50, "B": 50}
        assert model.is_active is is_active

    @pytest.mark.asyncio
    @pytest.mark.parametrize("is_active,fields", [
        (True, {"variants": {"A": 10, "B": 90}, "is_active": False}),  # выключение + смена
        (False, {"variants": {"A": 10, "B": 90}}),                       # смена у выключенного
        (False, {"variants": {"A": 10, "B": 90}, "is_active": False}),
        (True, {"variants": {"A": 50, "B": 50}}),                        # те же варианты — не изменение
        (True, {"is_active": False}),
        (True, {"url": "/p"}),
    ])
    async def test_change_allowed(self, is_active, fields):
        session, model = _fake_repo_session(is_active)
        result = await ExperimentRepository(session).update("test_exp_1", **fields)
        session.commit.assert_awaited_once()
        for key, value in fields.items():
            assert getattr(result, key) == value
        session.get.assert_awaited_once()
        assert session.get.await_args.kwargs.get("with_for_update") is True

    @pytest.mark.asyncio
    async def test_missing_experiment_none(self):
        session = MagicMock(name="FakeSession")
        session.get = AsyncMock(return_value=None)
        assert await ExperimentRepository(session).update("test_exp_404", is_active=False) is None
