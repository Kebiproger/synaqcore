"""Smoke: health, трекер событий, отсутствие CORS-заголовков, неизвестные пути."""
import time
import uuid

import pytest

from ._helpers import assert_no_cors, assert_status, json_of

TRACK = "/api/v1/tracker/events"


def _event(**overrides):
    event = {
        "event_id": uuid.uuid4().hex,
        "user_id": "user_test_1",
        "event_name": "smoke_click",
        "timestamp": int(time.time() * 1000),
        "experiment_name": "s_smoke",
        "variant": "A",
        "event_data": {"source": "smoke"},
    }
    event.update(overrides)
    return {k: v for k, v in event.items() if v is not None}


def test_health_ok(client):
    body = json_of(client.get("/health"))
    assert "status" in body


def test_track_event_accepted(client):
    body = json_of(client.post(TRACK, json=_event()))
    assert body == {"status": "OK"}


def test_track_event_without_user_id_returns_400(client):
    assert_status(client.post(TRACK, json=_event(user_id=None)), 400)


def test_track_event_with_stale_timestamp_returns_400(client):
    stale = int((time.time() - 30 * 24 * 3600) * 1000)
    assert_status(client.post(TRACK, json=_event(timestamp=stale)), 400)


def test_track_invalid_json_returns_400(client):
    r = client.post(TRACK, content=b"{not json", headers={"Content-Type": "application/json"})
    assert_status(r, 400)


@pytest.mark.parametrize(
    "method,path,body",
    [
        ("GET", "/health", None),
        ("GET", "/api/v1/variants/s_cors?user_id=user_test_1", None),
        ("GET", "/api/v1/variants/active", None),
        ("POST", "/api/v1/variants/batch", {"user_id": "user_test_1", "experiments": ["s_cors"]}),
        ("POST", TRACK, "event"),
        ("OPTIONS", "/api/v1/variants/s_cors", None),
        ("OPTIONS", TRACK, None),
    ],
)
def test_service_sends_no_cors_headers(client, method, path, body):
    headers = {"Origin": "https://example.com"}
    if method == "OPTIONS":
        headers["Access-Control-Request-Method"] = "POST"
    r = client.request(method, path, json=_event() if body == "event" else body, headers=headers)
    assert_no_cors(r)


def test_unknown_path_returns_404(client):
    assert_status(client.get("/api/v1/definitely-not-here"), 404)
