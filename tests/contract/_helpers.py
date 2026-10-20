"""Общие проверки smoke-тестов и эталонная аллокация."""
import hashlib

import httpx

BUCKETS = 10000


def expected_variant(user_id: str, experiment_name: str, variants: dict[str, int]) -> str:
    """Эталонное правило аллокации (копия, т.к. Python experiment_api уходит из lite):
    bucket = SHA-256("user_id:experiment_name") % 10000, варианты по алфавиту,
    первый с bucket * total < cumulative * 10000 (целочисленно)."""
    if not variants or any(w <= 0 for w in variants.values()):
        return "control"
    bucket = int(hashlib.sha256(f"{user_id}:{experiment_name}".encode()).hexdigest(), 16) % BUCKETS
    total = sum(variants.values())
    cumulative = 0
    for name, weight in sorted(variants.items()):
        cumulative += weight
        if bucket * total < cumulative * BUCKETS:
            return name
    return "control"


def _brief(r: httpx.Response) -> str:
    return f"{r.request.method} {r.request.url.path} → HTTP {r.status_code}: {r.text[:300]}"


def assert_status(r: httpx.Response, expected: int) -> None:
    assert r.status_code == expected, f"ожидался {expected}; {_brief(r)}"


def json_of(r: httpx.Response, expected: int = 200):
    assert_status(r, expected)
    try:
        return r.json()
    except ValueError:
        raise AssertionError(f"ответ не JSON; {_brief(r)}") from None


def assert_no_cors(r: httpx.Response) -> None:
    """CORS выставляет только nginx: сервис не должен отдавать Access-Control-*."""
    leaked = [h for h in r.headers if h.lower().startswith("access-control-")]
    assert not leaked, f"сервис отдал CORS-заголовки {leaked}; {_brief(r)}"


def assert_unauthorized(r: httpx.Response) -> None:
    assert_status(r, 401)
    assert r.headers.get("www-authenticate", "").startswith("Bearer"), f"нет WWW-Authenticate: Bearer; {_brief(r)}"
