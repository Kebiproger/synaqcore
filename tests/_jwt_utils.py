"""Вспомогательные функции для тестов OIDC/JWT (синтетические ключи и токены).

RSA-ключи генерируются один раз на процесс (≈ на сессию pytest). Никаких реальных
ключей/токенов — только сгенерированные в памяти.
"""
import base64
import functools
import hashlib
import hmac
import json
import time
import uuid
from typing import Any, Optional

import jwt
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import rsa

TEST_ISSUER = "https://auth.example.com"
TEST_AUDIENCE = "100000000000000001"          # синтетический project_id
TEST_JWKS_URL = "https://auth.example.com/oauth/v2/keys"
TEST_KID = "test_kid_1"
# Общий claim — сервисы его НЕ доверяют (может нести роли другого проекта)
GENERIC_ROLES_CLAIM = "urn:zitadel:iam:org:project:roles"


def project_roles_claim(project_id: str = TEST_AUDIENCE) -> str:
    return f"urn:zitadel:iam:org:project:{project_id}:roles"


# Проектный claim — единственный источник ролей
ROLES_CLAIM = project_roles_claim()


@functools.lru_cache(maxsize=None)
def rsa_key(name: str = "main") -> rsa.RSAPrivateKey:
    """Детерминированно-кэшированный (по имени) синтетический RSA-ключ."""
    return rsa.generate_private_key(public_exponent=65537, key_size=2048)


def public_jwk(key: rsa.RSAPrivateKey, kid: str = TEST_KID, **extra: Any) -> dict:
    jwk = json.loads(jwt.algorithms.RSAAlgorithm.to_jwk(key.public_key()))
    jwk["kid"] = kid
    jwk.update(extra)
    return jwk


def jwks(*jwks_list: dict) -> dict:
    return {"keys": list(jwks_list)}


def public_pem(key: rsa.RSAPrivateKey) -> bytes:
    return key.public_key().public_bytes(
        serialization.Encoding.PEM, serialization.PublicFormat.SubjectPublicKeyInfo
    )


def claims(
    roles: Optional[list[str]] = None,
    *,
    sub: str = "test_user_1",
    issuer: str = TEST_ISSUER,
    audience: Any = TEST_AUDIENCE,
    lifetime: int = 300,
    drop: tuple[str, ...] = (),
    **extra: Any,
) -> dict:
    """Claims access token: как у Zitadel JWT access token, с jti.

    Роли кладутся в проектный claim; project_id = audience (если это строка).
    """
    now = int(time.time())
    payload: dict[str, Any] = {
        "sub": sub,
        "iss": issuer,
        "aud": audience,
        "iat": now,
        "exp": now + lifetime,
        "jti": uuid.uuid4().hex,
    }
    if roles is not None:
        project_id = audience if isinstance(audience, str) else TEST_AUDIENCE
        payload[project_roles_claim(project_id)] = {r: {"100000000000000002": "test.example.com"} for r in roles}
    payload.update(extra)
    for name in drop:
        payload.pop(name, None)
    return payload


def make_token(
    roles: Optional[list[str]] = None,
    *,
    key: Optional[rsa.RSAPrivateKey] = None,
    kid: Optional[str] = TEST_KID,
    payload: Optional[dict] = None,
    **claim_kwargs: Any,
) -> str:
    """Подписанный RS256 access token в стиле Zitadel."""
    headers = {"kid": kid} if kid is not None else {}
    body = payload if payload is not None else claims(roles, **claim_kwargs)
    return jwt.encode(body, key or rsa_key(), algorithm="RS256", headers=headers)


def _b64(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()


def hs256_with_public_pem(roles: list[str], header_alg: str = "HS256") -> str:
    """Атака algorithm confusion: HMAC-подпись публичным PEM-ключом как секретом.

    PyJWT сам такой токен не выпустит, поэтому собираем вручную.
    """
    header = {"alg": header_alg, "typ": "JWT", "kid": TEST_KID}
    signing_input = f"{_b64(json.dumps(header).encode())}.{_b64(json.dumps(claims(roles)).encode())}"
    sig = hmac.new(public_pem(rsa_key()), signing_input.encode(), hashlib.sha256).digest()
    return f"{signing_input}.{_b64(sig)}"


def none_alg_token(roles: list[str]) -> str:
    return jwt.encode(claims(roles), None, algorithm="none", headers={"kid": TEST_KID})


def bearer(token: str) -> dict:
    return {"Authorization": f"Bearer {token}"}


class FakeClock:
    """Подмена модуля time в auth (используется только time.monotonic)."""

    def __init__(self, start: float = 1000.0):
        self.now = start

    def monotonic(self) -> float:
        return self.now

    def advance(self, seconds: float) -> None:
        self.now += seconds
