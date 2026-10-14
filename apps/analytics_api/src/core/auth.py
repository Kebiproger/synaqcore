"""Аутентификация через Zitadel OIDC (JWT access token + JWKS) и проверка ролей.

Копия apps/experiment_api/src/core/auth.py (сервисы деплоятся независимо).
При изменении логики синхронизируйте оба файла.

Как это работает:
  1. Клиент присылает `Authorization: Bearer <JWT>` (access token от Zitadel).
  2. Публичные ключи берём из JWKS (OIDC_JWKS_URL) и кэшируем в памяти с TTL.
     Zitadel выбирает инстанс по заголовку Host, поэтому при запросе по внутреннему
     адресу отправляем Host = OIDC_JWKS_HOST.
  3. Проверяем подпись (только RS256), exp/nbf/iat, iss, aud и наличие jti
     (у access token Zitadel jti есть, у ID token — нет, поэтому ID token
     как bearer-токен не принимается).
  4. Роли берём ТОЛЬКО из проектного claim `urn:zitadel:iam:org:project:<project_id>:roles`,
     где project_id = audience. Общий `urn:zitadel:iam:org:project:roles` игнорируется:
     он может содержать роли другого проекта (cross-project role confusion).
     Zitadel выдаёт проектный claim при запросе scope `urn:zitadel:iam:org:projects:roles`.

Fail closed: если OIDC не настроен (нет issuer / JWKS / audience) — 503,
если JWKS недоступен и ключей в кэше нет (или они старше JWKS_MAX_STALE_SECONDS) — тоже 503.
"""

import asyncio
import json
import logging
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Optional

import httpx
import jwt
from fastapi import Depends, HTTPException, status
from fastapi.security import HTTPAuthorizationCredentials, HTTPBearer

from src.core.config import settings

logger = logging.getLogger(__name__)

# ─────────── Параметры ───────────
ALGORITHMS = ["RS256"]                # none / HS* отклоняются
JWKS_TTL_SECONDS = 600                # как долго считаем ключи свежими
JWKS_MIN_REFRESH_INTERVAL = 30        # не чаще одного перезапроса JWKS (защита от DoS неизвестными kid)
JWKS_EMPTY_RETRY_INTERVAL = 5         # повтор, если ключей ещё нет совсем (Zitadel только стартует)
JWKS_MAX_STALE_SECONDS = 24 * 3600    # дольше устаревшие ключи не используем (JWKS недоступен) → 503
JWKS_FETCH_TIMEOUT = 3.0
LEEWAY_SECONDS = 30                   # допуск рассинхрона часов
MAX_TOKEN_LENGTH = 8192

_PROJECT_ROLES_PREFIX = "urn:zitadel:iam:org:project:"
_PROJECT_ROLES_SUFFIX = ":roles"

_bearer = HTTPBearer(auto_error=False)


@dataclass
class Principal:
    """Аутентифицированный пользователь/сервис."""
    sub: str
    roles: set[str] = field(default_factory=set)


# ─────────── Кэши модуля ───────────
_jwks_keys: dict[str, jwt.PyJWK] = {}
_jwks_fetched_at: float = 0.0
_jwks_last_attempt: float = 0.0
# Общий для всех запросов незавершённый запрос JWKS. Задача привязана к своему event loop
# (у каждого TestClient / воркера он свой), поэтому храним и loop.
_jwks_refresh_task: Optional[asyncio.Task] = None
_jwks_refresh_loop: Optional[asyncio.AbstractEventLoop] = None
_oidc_file_config: Optional[dict[str, str]] = None
_warned: set[str] = set()


def reset_auth_caches() -> None:
    """Сбросить все кэши (JWKS, конфиг из файла, флаги логов). Используется в тестах."""
    global _jwks_keys, _jwks_fetched_at, _jwks_last_attempt, _oidc_file_config
    global _jwks_refresh_task, _jwks_refresh_loop
    _jwks_keys = {}
    _jwks_fetched_at = 0.0
    _jwks_last_attempt = 0.0
    _jwks_refresh_task = None
    _jwks_refresh_loop = None
    _oidc_file_config = None
    _warned.clear()


def _warn_once(key: str, message: str, *args: Any) -> None:
    if key in _warned:
        return
    _warned.add(key)
    logger.warning(message, *args)


# ─────────── Ошибки ───────────
def _unauthorized() -> HTTPException:
    return HTTPException(
        status_code=status.HTTP_401_UNAUTHORIZED,
        detail="Требуется аутентификация",
        headers={"WWW-Authenticate": "Bearer"},
    )


def _not_configured() -> HTTPException:
    return HTTPException(
        status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
        detail="Аутентификация не настроена",
    )


def _unavailable() -> HTTPException:
    return HTTPException(
        status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
        detail="Сервис аутентификации недоступен",
    )


# ─────────── Конфигурация OIDC ───────────
def _load_oidc_file_config() -> Optional[dict[str, str]]:
    """Прочитать OIDC_CONFIG_FILE (пишется bootstrap-контейнером после старта).

    Файла может ещё не быть — читаем лениво и кэшируем, как только он валиден.
    """
    global _oidc_file_config
    if _oidc_file_config is not None:
        return _oidc_file_config
    path_str = settings.OIDC_CONFIG_FILE
    if not path_str:
        return None
    path = Path(path_str)
    try:
        if not path.is_file():
            _warn_once("config_file_missing", "Файл OIDC-конфига пока отсутствует: %s", path_str)
            return None
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError) as e:
        _warn_once("config_file_invalid", "Не удалось прочитать OIDC-конфиг %s: %s", path_str, type(e).__name__)
        return None
    if not isinstance(data, dict):
        _warn_once("config_file_invalid", "OIDC-конфиг %s имеет неверный формат", path_str)
        return None
    cfg = {k: v for k, v in data.items() if k in ("issuer", "project_id", "client_id") and isinstance(v, str)}
    if not cfg.get("project_id"):
        _warn_once("config_file_invalid", "В OIDC-конфиге %s нет project_id", path_str)
        return None
    _oidc_file_config = cfg
    logger.info("OIDC-конфиг загружен из %s", path_str)
    return cfg


def _resolve_oidc_settings() -> tuple[str, str]:
    """Вернуть (issuer, audience) или 503, если что-то не настроено."""
    issuer = settings.OIDC_ISSUER
    audience = settings.OIDC_AUDIENCE
    if not (issuer and audience):
        file_cfg = _load_oidc_file_config() or {}
        issuer = issuer or file_cfg.get("issuer", "")
        audience = audience or file_cfg.get("project_id", "")
    if not (issuer and audience and settings.OIDC_JWKS_URL):
        _warn_once(
            "not_configured",
            "OIDC не настроен (issuer=%s, audience=%s, jwks_url=%s) — защищённые эндпоинты отвечают 503",
            bool(issuer), bool(audience), bool(settings.OIDC_JWKS_URL),
        )
        raise _not_configured()
    return issuer, audience


# ─────────── JWKS ───────────
async def fetch_jwks() -> dict[str, Any]:
    """Загрузить JWKS из Zitadel. В тестах подменяется (patch src.core.auth.fetch_jwks)."""
    headers = {"Accept": "application/json"}
    if settings.OIDC_JWKS_HOST:
        headers["Host"] = settings.OIDC_JWKS_HOST
    async with httpx.AsyncClient(timeout=JWKS_FETCH_TIMEOUT, follow_redirects=False) as client:
        resp = await client.get(settings.OIDC_JWKS_URL, headers=headers)
        resp.raise_for_status()
        return resp.json()


def _parse_jwks(data: Any) -> dict[str, jwt.PyJWK]:
    keys: dict[str, jwt.PyJWK] = {}
    if not isinstance(data, dict) or not isinstance(data.get("keys"), list):
        return keys
    for jwk in data["keys"]:
        if not isinstance(jwk, dict):
            continue
        kid = jwk.get("kid")
        if not isinstance(kid, str) or not kid:
            continue
        if jwk.get("kty") != "RSA":
            continue
        if jwk.get("use") not in (None, "sig"):
            continue
        if jwk.get("alg") not in (None, "RS256"):
            continue
        try:
            keys[kid] = jwt.PyJWK(jwk, algorithm="RS256")
        except Exception:
            continue
    return keys


async def _refresh_jwks() -> None:
    """Один запрос JWKS; при успехе заменяет кэш ключей. Ошибки не пробрасывает."""
    global _jwks_keys, _jwks_fetched_at
    try:
        parsed = _parse_jwks(await fetch_jwks())
    except Exception as e:
        logger.warning("Не удалось получить JWKS: %s", type(e).__name__)
        return
    if parsed:
        _jwks_keys = parsed
        _jwks_fetched_at = time.monotonic()
    else:
        logger.warning("JWKS не содержит подходящих RS256-ключей")


async def _refresh_shared(allow_start: bool) -> None:
    """Дождаться уже идущего запроса JWKS или (если allow_start) запустить новый.

    Параллельные запросы ждут одну и ту же задачу — нет ни шторма запросов к Zitadel,
    ни всплеска 503 на холодном старте. shield: отмена одного HTTP-запроса
    не отменяет общую загрузку ключей.
    """
    global _jwks_last_attempt, _jwks_refresh_task, _jwks_refresh_loop
    loop = asyncio.get_running_loop()
    task = _jwks_refresh_task
    if task is not None and not task.done() and _jwks_refresh_loop is loop:
        await asyncio.shield(task)
        return
    if not allow_start:
        return
    # Отмечаем попытку до await, чтобы следующие запросы попали под rate limit
    _jwks_last_attempt = time.monotonic()
    task = loop.create_task(_refresh_jwks())
    _jwks_refresh_task, _jwks_refresh_loop = task, loop
    await asyncio.shield(task)


async def _get_signing_key(kid: str) -> Optional[jwt.PyJWK]:
    """Ключ по kid из кэша; при промахе/истечении TTL — перезапрос (с ограничением частоты)."""
    now = time.monotonic()
    fresh = bool(_jwks_keys) and (now - _jwks_fetched_at) < JWKS_TTL_SECONDS
    if fresh and kid in _jwks_keys:
        return _jwks_keys[kid]

    interval = JWKS_MIN_REFRESH_INTERVAL if _jwks_keys else JWKS_EMPTY_RETRY_INTERVAL
    allow_start = _jwks_last_attempt == 0.0 or (now - _jwks_last_attempt) >= interval
    await _refresh_shared(allow_start)

    if not _jwks_keys:
        raise _unavailable()
    if time.monotonic() - _jwks_fetched_at >= JWKS_MAX_STALE_SECONDS:
        _warn_once("jwks_too_stale", "Ключи JWKS устарели более чем на %d c — отвечаем 503", JWKS_MAX_STALE_SECONDS)
        raise _unavailable()
    # Если обновить не удалось — используем устаревшие (но не старше суток) ключи, чем отказывать всем
    return _jwks_keys.get(kid)


# ─────────── Проверка токена ───────────
def _extract_roles(claims: dict[str, Any], project_id: str) -> set[str]:
    """Роли только из claim своего проекта.

    Форматы значения: {"role": {...}} или список таких объектов [{"role": {...}}, ...].
    """
    value = claims.get(f"{_PROJECT_ROLES_PREFIX}{project_id}{_PROJECT_ROLES_SUFFIX}")
    items = value if isinstance(value, list) else [value]
    roles: set[str] = set()
    for item in items:
        if isinstance(item, dict):
            roles.update(r for r in item if isinstance(r, str) and r)
    return roles


async def verify_token(token: str) -> Principal:
    """Проверить JWT и вернуть Principal. 401 — невалидный токен, 503 — OIDC не настроен.

    Заголовок токена проверяем до чтения настроек: мусор вместо токена — всегда 401.
    """
    if len(token) > MAX_TOKEN_LENGTH:
        raise _unauthorized()
    try:
        header = jwt.get_unverified_header(token)
    except jwt.PyJWTError:
        raise _unauthorized()
    if header.get("alg") not in ALGORITHMS:
        raise _unauthorized()
    kid = header.get("kid")
    if not isinstance(kid, str) or not kid:
        raise _unauthorized()

    issuer, audience = _resolve_oidc_settings()
    signing_key = await _get_signing_key(kid)
    if signing_key is None:
        raise _unauthorized()

    try:
        claims = jwt.decode(
            token,
            key=signing_key.key,
            algorithms=ALGORITHMS,
            audience=audience,
            issuer=issuer,
            leeway=LEEWAY_SECONDS,
            options={"require": ["exp", "iat", "iss", "aud", "sub", "jti"]},
        )
    except jwt.PyJWTError as e:
        logger.info("Отклонён JWT: %s", type(e).__name__)
        raise _unauthorized()

    sub = claims.get("sub")
    if not isinstance(sub, str) or not sub:
        raise _unauthorized()
    return Principal(sub=sub, roles=_extract_roles(claims, audience))


# ─────────── FastAPI-зависимости ───────────
async def get_current_principal(
    credentials: Optional[HTTPAuthorizationCredentials] = Depends(_bearer),
) -> Principal:
    if credentials is None or not credentials.credentials:
        raise _unauthorized()
    return await verify_token(credentials.credentials)


def require_roles(*roles: str):
    """Фабрика зависимостей: нужна хотя бы одна из перечисленных ролей, иначе 403."""
    if not roles:
        raise ValueError("require_roles: нужно указать хотя бы одну роль")
    required = frozenset(roles)

    async def _dependency(principal: Principal = Depends(get_current_principal)) -> Principal:
        if principal.roles.isdisjoint(required):
            raise HTTPException(status_code=status.HTTP_403_FORBIDDEN, detail="Недостаточно прав")
        return principal

    return _dependency
