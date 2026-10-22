"""Python Server SDK (sdk/python) против golden-векторов аллокации сервера.

Эталон — apps/synaq/internal/allocation/testdata/golden.json (по нему же
проверяется Go-сервис synaq). Ключи вариантов подаются в обратном порядке:
результат не должен зависеть от порядка ключей (JSONB его не сохраняет).
"""
import json
from pathlib import Path

import pytest

from sdk.python.synaq import SynaqServerClient

GOLDEN = (
    Path(__file__).resolve().parent.parent
    / "apps" / "synaq" / "internal" / "allocation" / "testdata" / "golden.json"
)
VECTORS = json.loads(GOLDEN.read_text(encoding="utf-8"))


@pytest.fixture(scope="module")
def client():
    return SynaqServerClient(base_url="http://fake-host:8080", auto_start=False)


def test_golden_vectors_loaded():
    assert len(VECTORS) >= 300


def test_sdk_matches_golden(client):
    mismatches = []
    for v in VECTORS:
        if not v["user_id"]:
            continue  # пустой user_id SDK сознательно не считает: отдаёт fallback
        variants = dict(reversed(list(v["variants"].items())))
        client._experiments = {v["experiment"]: {"name": v["experiment"], "variants": variants, "is_active": True}}
        got = client.get_variant(v["experiment"], v["user_id"])
        if got != v["want"]:
            mismatches.append((v["source"], v["user_id"], v["experiment"], got, v["want"]))
    assert not mismatches, f"{len(mismatches)} расхождений, первые: {mismatches[:5]}"


def test_fallback_on_missing_experiment(client):
    client._experiments = {}
    assert client.get_variant("non_existent_test", "usr_1") == "control"
