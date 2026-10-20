"""Smoke-тесты HTTP-API Go-сервиса synaq (apps/synaq) по одному базовому URL.

Запуск:  tests/contract/run.sh [доп. аргументы pytest]
Вручную: SYNAQ_SMOKE=1 ./venv/bin/python -m pytest tests/contract
         (стенд tests/contract/docker-compose.contract.yml должен быть поднят,
         базовый URL — SYNAQ_BASE_URL, по умолчанию http://127.0.0.1:18002).

Без SYNAQ_SMOKE=1 тесты пропускаются: обычный прогон `pytest tests/` не требует
docker-стенда.

Ключ RSA генерируется в памяти на каждую сессию; в каталог .runtime пишется только
публичный jwks.json, который раздаёт сервис `jwks` стенда. Все данные синтетические.
"""
from __future__ import annotations

import itertools
import json
import os
import secrets
import time
import uuid
from pathlib import Path
from typing import Any, Callable, Optional

import httpx
import jwt
import pytest
from cryptography.hazmat.primitives.asymmetric import rsa

CONTRACT_DIR = Path(__file__).resolve().parent
RUNTIME_DIR = Path(os.environ.get("SYNAQ_RUNTIME_DIR", CONTRACT_DIR / ".runtime"))

BASE_URL = os.environ.get("SYNAQ_BASE_URL", "http://127.0.0.1:18002").rstrip("/")

# Должны совпадать с x-test-oidc в docker-compose.contract.yml
OIDC_ISSUER = os.environ.get("SYNAQ_OIDC_ISSUER", "http://contract-issuer.test")
OIDC_AUDIENCE = os.environ.get("SYNAQ_OIDC_AUDIENCE", "000000000000000001")
ROLES_CLAIM = f"urn:zitadel:iam:org:project:{OIDC_AUDIENCE}:roles"

HEALTH_TIMEOUT = float(os.environ.get("SYNAQ_HEALTH_TIMEOUT", "90"))
# JWKS кэшируется сервисом и перезапрашивается с ограничением частоты: после смены
# ключа (новая сессия pytest на том же стенде) первый успешный запрос может ждать до ~35 с.
AUTH_WARMUP_TIMEOUT = float(os.environ.get("SYNAQ_AUTH_WARMUP_TIMEOUT", "45"))

# Уникальный идентификатор прогона: имена экспериментов не пересекаются между прогонами
RUN_ID = secrets.token_hex(3)


def _enabled() -> bool:
    return os.environ.get("SYNAQ_SMOKE") == "1"


def pytest_collection_modifyitems(config, items):
    if _enabled():
        return
    skip = pytest.mark.skip(reason="smoke-тесты synaq: запуск через tests/contract/run.sh или SYNAQ_SMOKE=1")
    for item in items:
        if CONTRACT_DIR in Path(str(item.fspath)).resolve().parents:
            item.add_marker(skip)


# ─────────── Ключи и токены ───────────
class TokenFactory:
    """Выпускает RS256 access token в формате Zitadel (роли в проектном claim)."""

    def __init__(self) -> None:
        self.kid = f"smoke-{RUN_ID}"
        self._key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
        # Чужой ключ — для проверки «неверной подписи» (тот же kid)
        self._foreign_key = rsa.generate_private_key(public_exponent=65537, key_size=2048)

    def jwks(self) -> dict[str, Any]:
        jwk = json.loads(jwt.algorithms.RSAAlgorithm.to_jwk(self._key.public_key()))
        jwk.update({"kid": self.kid, "alg": "RS256", "use": "sig"})
        return {"keys": [jwk]}

    def claims(self, roles: tuple[str, ...] = ("admin",), **overrides: Any) -> dict[str, Any]:
        now = int(time.time())
        claims: dict[str, Any] = {
            "iss": OIDC_ISSUER,
            "aud": [OIDC_AUDIENCE],
            "sub": f"smoke-user-{RUN_ID}",
            "iat": now,
            "nbf": now,
            "exp": now + 3600,
            "jti": uuid.uuid4().hex,
        }
        if roles:
            claims[ROLES_CLAIM] = {r: {"000000000000000002": "smoke.example.com"} for r in roles}
        for k, v in overrides.items():
            if v is None:
                claims.pop(k, None)  # None — убрать claim
            else:
                claims[k] = v
        return claims

    def token(self, roles: tuple[str, ...] = ("admin",), *, foreign_key: bool = False, **claim_overrides: Any) -> str:
        key = self._foreign_key if foreign_key else self._key
        return jwt.encode(self.claims(roles, **claim_overrides), key, algorithm="RS256", headers={"kid": self.kid})

    def bearer(self, roles: tuple[str, ...] = ("admin",), **kwargs: Any) -> dict[str, str]:
        return {"Authorization": f"Bearer {self.token(roles, **kwargs)}"}


@pytest.fixture(scope="session")
def tokens() -> TokenFactory:
    factory = TokenFactory()
    RUNTIME_DIR.mkdir(parents=True, exist_ok=True)
    path = RUNTIME_DIR / "jwks.json"
    tmp = path.with_suffix(".tmp")
    tmp.write_text(json.dumps(factory.jwks()), encoding="utf-8")
    tmp.chmod(0o644)
    tmp.replace(path)  # атомарно: сервис не прочитает наполовину записанный файл
    return factory


@pytest.fixture(scope="session")
def admin_headers(tokens) -> dict[str, str]:
    return tokens.bearer(("admin",))


@pytest.fixture(scope="session")
def analyst_headers(tokens) -> dict[str, str]:
    return tokens.bearer(("analyst",))


@pytest.fixture(scope="session")
def norole_headers(tokens) -> dict[str, str]:
    return tokens.bearer(())


# ─────────── Клиент ───────────
def _wait_healthy(base_url: str) -> None:
    deadline = time.monotonic() + HEALTH_TIMEOUT
    last = "нет ответа"
    while time.monotonic() < deadline:
        try:
            r = httpx.get(f"{base_url}/health", timeout=3)
            if r.status_code == 200:
                return
            last = f"HTTP {r.status_code}"
        except httpx.HTTPError as e:
            last = type(e).__name__
        time.sleep(1)
    pytest.exit(f"synaq ({base_url}) не стал healthy за {HEALTH_TIMEOUT:.0f} с: {last}", returncode=3)


def _warmup_auth(client: httpx.Client, headers: dict[str, str]) -> None:
    """Дождаться, пока сервис подхватит свежий JWKS (401/503 → ждём).

    Любой другой статус считаем «готов» (тесты сами покажут ошибку); если за
    таймаут ответ так и не изменился — продолжаем: упадут тесты с понятным статусом.
    """
    deadline = time.monotonic() + AUTH_WARMUP_TIMEOUT
    while time.monotonic() < deadline:
        try:
            r = client.get("/api/v1/experiments/", headers=headers)
            if r.status_code not in (401, 503):
                return
        except httpx.HTTPError:
            pass
        time.sleep(1)


@pytest.fixture(scope="session")
def client(tokens, admin_headers):
    _wait_healthy(BASE_URL)
    # Редиректы не следуем: статусы/Location проверяются как есть
    with httpx.Client(base_url=BASE_URL, timeout=15, follow_redirects=False) as c:
        _warmup_auth(c, admin_headers)
        yield c


# ─────────── Уникальные имена и эксперименты ───────────
_name_counter = itertools.count(1)


@pytest.fixture
def exp_name() -> Callable[..., str]:
    """Фабрика уникальных имён экспериментов: s<run>_<n>_<suffix> (≤ 50 символов)."""
    def make(suffix: str = "e") -> str:
        name = f"s{RUN_ID}_{next(_name_counter)}_{suffix}"
        assert len(name) <= 50, name
        return name
    return make


@pytest.fixture
def make_experiment(client, admin_headers, exp_name):
    """Создать эксперимент через API (ожидается 201) и вернуть имя; удаляется после теста."""
    created: list[str] = []

    def make(variants: Optional[dict[str, int]] = None, **extra: Any) -> str:
        name = extra.pop("name", None) or exp_name()
        body = {"name": name, "variants": variants or {"control": 50, "treatment": 50}, **extra}
        r = client.post("/api/v1/experiments/", json=body, headers=admin_headers)
        assert r.status_code == 201, f"создание {name}: HTTP {r.status_code} {r.text[:300]}"
        created.append(name)
        return name

    yield make
    for name in created:
        try:
            client.delete(f"/api/v1/experiments/{name}", headers=admin_headers)
        except httpx.HTTPError:
            pass
