"""Тесты отказоустойчивости Client/Admin API при сбоях Redis.

Проверяем:
  - Redis недоступен при чтении → fallback на PostgreSQL, кэш не пишется
  - Redis промах + сбой записи → запрос всё равно успешен
  - Redis hit → PostgreSQL не опрашивается
  - Сбой PostgreSQL → ошибка не проглатывается
  - /variants/active не перехватывается маршрутом /{experiment_name}
  - Admin create/delete успешны при сбое Redis
  - Детерминизм аллокации не зависит от источника конфига

Lifespan приложения не используется: собираем свежий FastAPI() с роутерами,
get_session и get_current_principal (OIDC) переопределяются. Все данные синтетические.
"""

from unittest.mock import AsyncMock, MagicMock, patch

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from src.api.v1 import admin as admin_module
from src.api.v1 import client as client_module
from src.core.auth import Principal, get_current_principal
from src.core.database import get_session
from src.schemas.experiment import ExperimentConfigCreate
from src.services.allocation import get_user_variant


EXP_NAME = "test_exp_1"
DB_CONFIG = ExperimentConfigCreate(
    name=EXP_NAME,
    variants={"A": 50, "B": 50},
    config={"A": {"color": "red"}, "B": {"color": "blue"}},
    url="/test-page",
    is_active=True,
)


async def _fake_session():
    yield MagicMock(name="FakeSession")


def _build_app() -> FastAPI:
    app = FastAPI()
    app.include_router(client_module.router, prefix="/api/v1")
    app.include_router(admin_module.router, prefix="/api/v1")
    app.dependency_overrides[get_session] = _fake_session
    app.dependency_overrides[get_current_principal] = lambda: Principal(sub="test_admin", roles={"admin"})
    return app


@pytest.fixture
def http():
    return TestClient(_build_app(), raise_server_exceptions=False)


@pytest.fixture
def http_raising():
    return TestClient(_build_app(), raise_server_exceptions=True)


def _repo_mock(get_return=None, get_side_effect=None, list_all=None):
    """Мок класса ExperimentRepository: возвращает экземпляр с AsyncMock-методами."""
    instance = MagicMock(name="RepoInstance")
    instance.get = AsyncMock(return_value=get_return, side_effect=get_side_effect)
    instance.list_all = AsyncMock(return_value=list_all or [])
    instance.exists = AsyncMock(return_value=False)
    instance.create = AsyncMock(return_value=None)
    instance.delete = AsyncMock(return_value=True)
    cls = MagicMock(name="RepoClass", return_value=instance)
    return cls, instance


def _redis_mock(get_side_effect=None, get_return=None, save_side_effect=None, delete_side_effect=None):
    cache = MagicMock(name="RedisCache")
    cache.get_experiment = AsyncMock(return_value=get_return, side_effect=get_side_effect)
    cache.save_experiment = AsyncMock(side_effect=save_side_effect)
    cache.delete_experiment = AsyncMock(side_effect=delete_side_effect)
    return cache


# ────────────────────────────────
# 1-2. Redis read fails → fallback на PostgreSQL
# ────────────────────────────────

class TestRedisReadFailure:

    @pytest.mark.parametrize("exc", [ConnectionError("redis down"), RuntimeError("Redis не инициализирован")])
    def test_variant_falls_back_to_db(self, http, exc):
        repo_cls, repo = _repo_mock(get_return=DB_CONFIG)
        cache = _redis_mock(get_side_effect=exc)
        with patch.object(client_module, "redis_cache", cache), \
             patch.object(client_module, "ExperimentRepository", repo_cls):
            resp = http.get(f"/api/v1/variants/{EXP_NAME}", params={"user_id": "user_test_1"})

        assert resp.status_code == 200
        body = resp.json()
        assert body["experiment_name"] == EXP_NAME
        assert body["user_id"] == "user_test_1"
        assert body["variant"] in DB_CONFIG.variants
        repo.get.assert_awaited_once_with(EXP_NAME)
        cache.save_experiment.assert_not_called()

    @pytest.mark.parametrize("exc", [ConnectionError("redis down"), RuntimeError("Redis не инициализирован")])
    def test_config_falls_back_to_db(self, http, exc):
        repo_cls, repo = _repo_mock(get_return=DB_CONFIG)
        cache = _redis_mock(get_side_effect=exc)
        with patch.object(client_module, "redis_cache", cache), \
             patch.object(client_module, "ExperimentRepository", repo_cls):
            resp = http.get(f"/api/v1/variants/config/{EXP_NAME}")

        assert resp.status_code == 200
        body = resp.json()
        assert body == {
            "name": EXP_NAME,
            "variants": {"A": 50, "B": 50},
            "config": {"A": {"color": "red"}, "B": {"color": "blue"}},
            "url": "/test-page",
            "is_active": True,
        }
        repo.get.assert_awaited_once_with(EXP_NAME)
        cache.save_experiment.assert_not_called()

    def test_config_404_when_redis_down_and_not_in_db(self, http):
        repo_cls, _ = _repo_mock(get_return=None)
        cache = _redis_mock(get_side_effect=ConnectionError("redis down"))
        with patch.object(client_module, "redis_cache", cache), \
             patch.object(client_module, "ExperimentRepository", repo_cls):
            resp = http.get("/api/v1/variants/config/missing_exp")
        assert resp.status_code == 404

    def test_variant_control_when_redis_down_and_not_in_db(self, http):
        repo_cls, _ = _repo_mock(get_return=None)
        cache = _redis_mock(get_side_effect=ConnectionError("redis down"))
        with patch.object(client_module, "redis_cache", cache), \
             patch.object(client_module, "ExperimentRepository", repo_cls):
            resp = http.get("/api/v1/variants/missing_exp", params={"user_id": "user_test_1"})
        assert resp.status_code == 200
        assert resp.json()["variant"] == "control"


# ────────────────────────────────
# 3. Redis miss, запись в кэш падает
# ────────────────────────────────

class TestRedisWriteFailure:

    def test_variant_ok_when_save_fails(self, http):
        repo_cls, _ = _repo_mock(get_return=DB_CONFIG)
        cache = _redis_mock(get_return=None, save_side_effect=ConnectionError("redis down"))
        with patch.object(client_module, "redis_cache", cache), \
             patch.object(client_module, "ExperimentRepository", repo_cls):
            resp = http.get(f"/api/v1/variants/{EXP_NAME}", params={"user_id": "user_test_2"})
        assert resp.status_code == 200
        assert resp.json()["variant"] in DB_CONFIG.variants
        cache.save_experiment.assert_awaited_once_with(DB_CONFIG)

    def test_config_ok_when_save_fails(self, http):
        repo_cls, _ = _repo_mock(get_return=DB_CONFIG)
        cache = _redis_mock(get_return=None, save_side_effect=RuntimeError("boom"))
        with patch.object(client_module, "redis_cache", cache), \
             patch.object(client_module, "ExperimentRepository", repo_cls):
            resp = http.get(f"/api/v1/variants/config/{EXP_NAME}")
        assert resp.status_code == 200
        assert resp.json()["name"] == EXP_NAME
        cache.save_experiment.assert_awaited_once()

    def test_miss_caches_config_when_redis_healthy(self, http):
        repo_cls, _ = _repo_mock(get_return=DB_CONFIG)
        cache = _redis_mock(get_return=None)
        with patch.object(client_module, "redis_cache", cache), \
             patch.object(client_module, "ExperimentRepository", repo_cls):
            resp = http.get(f"/api/v1/variants/{EXP_NAME}", params={"user_id": "user_test_3"})
        assert resp.status_code == 200
        cache.save_experiment.assert_awaited_once_with(DB_CONFIG)


# ────────────────────────────────
# 4. Redis hit → БД не опрашивается
# ────────────────────────────────

class TestRedisHit:

    def test_hit_skips_db(self, http):
        repo_cls, repo = _repo_mock(get_return=DB_CONFIG)
        cache = _redis_mock(get_return=DB_CONFIG)
        with patch.object(client_module, "redis_cache", cache), \
             patch.object(client_module, "ExperimentRepository", repo_cls):
            r1 = http.get(f"/api/v1/variants/{EXP_NAME}", params={"user_id": "user_test_4"})
            r2 = http.get(f"/api/v1/variants/config/{EXP_NAME}")
        assert r1.status_code == 200
        assert r2.status_code == 200
        repo.get.assert_not_called()
        cache.save_experiment.assert_not_called()


# ────────────────────────────────
# 5. Сбой PostgreSQL не проглатывается
# ────────────────────────────────

class TestDbFailure:

    class DbDown(Exception):
        pass

    def test_db_error_returns_500(self, http):
        repo_cls, _ = _repo_mock(get_side_effect=self.DbDown("postgres down"))
        cache = _redis_mock(get_return=None)
        with patch.object(client_module, "redis_cache", cache), \
             patch.object(client_module, "ExperimentRepository", repo_cls):
            resp = http.get(f"/api/v1/variants/{EXP_NAME}", params={"user_id": "user_test_5"})
        assert resp.status_code == 500

    def test_db_error_propagates_when_redis_also_down(self, http_raising):
        repo_cls, _ = _repo_mock(get_side_effect=self.DbDown("postgres down"))
        cache = _redis_mock(get_side_effect=ConnectionError("redis down"))
        with patch.object(client_module, "redis_cache", cache), \
             patch.object(client_module, "ExperimentRepository", repo_cls):
            with pytest.raises(self.DbDown):
                http_raising.get(f"/api/v1/variants/config/{EXP_NAME}")


# ────────────────────────────────
# 6. /variants/active не затеняется /{experiment_name}
# ────────────────────────────────

class TestActiveList:

    def test_active_returns_only_active(self, http):
        exps = [
            ExperimentConfigCreate(name="test_exp_on", variants={"A": 50, "B": 50}, url="/p1", is_active=True),
            ExperimentConfigCreate(name="test_exp_off", variants={"A": 1}, is_active=False),
            ExperimentConfigCreate(name="test_exp_on2", variants={"X": 1, "Y": 2, "Z": 3}, is_active=True),
        ]
        repo_cls, repo = _repo_mock(list_all=exps)
        cache = _redis_mock()
        with patch.object(client_module, "redis_cache", cache), \
             patch.object(client_module, "ExperimentRepository", repo_cls):
            resp = http.get("/api/v1/variants/active")

        assert resp.status_code == 200
        assert resp.json() == [
            {"name": "test_exp_on", "url": "/p1", "variants": ["A", "B"]},
            {"name": "test_exp_on2", "url": None, "variants": ["X", "Y", "Z"]},
        ]
        # Не трактуется как эксперимент с именем "active"
        cache.get_experiment.assert_not_called()
        repo.get.assert_not_called()
        repo.list_all.assert_awaited_once()

    def test_active_empty(self, http):
        repo_cls, _ = _repo_mock(list_all=[])
        with patch.object(client_module, "redis_cache", _redis_mock()), \
             patch.object(client_module, "ExperimentRepository", repo_cls):
            resp = http.get("/api/v1/variants/active")
        assert resp.status_code == 200
        assert resp.json() == []


# ────────────────────────────────
# 7. Admin create/delete при сбое Redis
# ────────────────────────────────

class TestAdminRedisFailure:

    @pytest.mark.parametrize("exc", [ConnectionError("redis down"), RuntimeError("Redis не инициализирован")])
    def test_create_succeeds_when_redis_save_fails(self, http, exc):
        repo_cls, repo = _repo_mock()
        cache = _redis_mock(save_side_effect=exc)
        payload = {"name": "test_exp_new", "variants": {"A": 50, "B": 50}}
        with patch.object(admin_module, "redis_cache", cache), \
             patch.object(admin_module, "ExperimentRepository", repo_cls):
            resp = http.post("/api/v1/experiments/", json=payload)
        assert resp.status_code == 201
        assert resp.json()["name"] == "test_exp_new"
        repo.create.assert_awaited_once()
        cache.save_experiment.assert_awaited_once()

    @pytest.mark.parametrize("exc", [ConnectionError("redis down"), RuntimeError("Redis не инициализирован")])
    def test_delete_succeeds_when_redis_delete_fails(self, http, exc):
        repo_cls, repo = _repo_mock()
        cache = _redis_mock(delete_side_effect=exc)
        with patch.object(admin_module, "redis_cache", cache), \
             patch.object(admin_module, "ExperimentRepository", repo_cls):
            resp = http.delete("/api/v1/experiments/test_exp_old")
        assert resp.status_code == 200
        repo.delete.assert_awaited_once_with("test_exp_old")
        cache.delete_experiment.assert_awaited_once_with("test_exp_old")

    def test_create_duplicate_still_400(self, http):
        repo_cls, repo = _repo_mock()
        repo.exists = AsyncMock(return_value=True)
        cache = _redis_mock(save_side_effect=ConnectionError("redis down"))
        with patch.object(admin_module, "redis_cache", cache), \
             patch.object(admin_module, "ExperimentRepository", repo_cls):
            resp = http.post("/api/v1/experiments/", json={"name": "test_exp_dup", "variants": {"A": 1}})
        assert resp.status_code == 400
        repo.create.assert_not_called()

    def test_delete_missing_still_404(self, http):
        repo_cls, repo = _repo_mock()
        repo.delete = AsyncMock(return_value=False)
        cache = _redis_mock(delete_side_effect=ConnectionError("redis down"))
        with patch.object(admin_module, "redis_cache", cache), \
             patch.object(admin_module, "ExperimentRepository", repo_cls):
            resp = http.delete("/api/v1/experiments/test_exp_missing")
        assert resp.status_code == 404
        cache.delete_experiment.assert_not_called()


# ────────────────────────────────
# 8. Детерминизм: Redis down vs up
# ────────────────────────────────

class TestDeterminismAcrossSources:

    @pytest.mark.parametrize("user_id", [f"user_test_{i}" for i in range(20)])
    def test_same_variant_redis_down_vs_up(self, http, user_id):
        repo_cls, _ = _repo_mock(get_return=DB_CONFIG)

        with patch.object(client_module, "redis_cache", _redis_mock(get_return=DB_CONFIG)), \
             patch.object(client_module, "ExperimentRepository", repo_cls):
            up = http.get(f"/api/v1/variants/{EXP_NAME}", params={"user_id": user_id}).json()["variant"]

        with patch.object(client_module, "redis_cache", _redis_mock(get_side_effect=ConnectionError("down"))), \
             patch.object(client_module, "ExperimentRepository", repo_cls):
            down = http.get(f"/api/v1/variants/{EXP_NAME}", params={"user_id": user_id}).json()["variant"]

        assert up == down
        assert up == get_user_variant(user_id=user_id, experiment_name=EXP_NAME, variants=DB_CONFIG.variants)


# ────────────────────────────────
# 9. Пакетная выдача вариантов (POST /batch)
# ────────────────────────────────

class TestBatchEndpoint:

    def test_batch_variants_success(self, http):
        def _get_exp(name):
            return DB_CONFIG if name == EXP_NAME else None

        repo_cls, repo = _repo_mock()
        repo.get = AsyncMock(side_effect=_get_exp)
        cache = _redis_mock()
        cache.get_experiment = AsyncMock(side_effect=_get_exp)

        with patch.object(client_module, "redis_cache", cache), \
             patch.object(client_module, "ExperimentRepository", repo_cls):
            payload = {
                "user_id": "usr_batch_123",
                "experiments": [EXP_NAME, "non_existent_exp"]
            }
            resp = http.post("/api/v1/variants/batch", json=payload)
            assert resp.status_code == 200
            data = resp.json()
            assert data["user_id"] == "usr_batch_123"
            assert EXP_NAME in data["variants"]
            assert data["variants"][EXP_NAME] in ("A", "B")
            # Несуществующий должен вернуть control
            assert data["variants"]["non_existent_exp"] == "control"

    def test_batch_validation_errors(self, http):
        # Пустой список экспериментов
        resp = http.post("/api/v1/variants/batch", json={"user_id": "u1", "experiments": []})
        assert resp.status_code == 422

        # Пустой user_id
        resp = http.post("/api/v1/variants/batch", json={"user_id": "", "experiments": [EXP_NAME]})
        assert resp.status_code == 422

