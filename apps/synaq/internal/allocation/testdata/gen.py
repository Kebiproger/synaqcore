"""Генератор golden-векторов для internal/allocation (testdata/golden.json).

Ожидаемые значения считает сама серверная функция get_user_variant()
из apps/experiment_api/src/services/allocation.py — эталон, с которым Go-реализация
обязана совпадать бит в бит.

Запуск из корня репозитория:
    ./venv/bin/python -I apps/synaq/internal/allocation/testdata/gen.py

Генерация детерминирована (фиксированный seed): повторный запуск даёт тот же файл.
"""
import hashlib
import json
import random
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[4]
sys.path.insert(0, str(REPO / "apps" / "experiment_api"))

from src.services.allocation import BUCKETS, get_user_variant  # noqa: E402

MAX_INT64 = 2**63 - 1


def bucket_of(user_id: str, experiment: str) -> int:
    digest = hashlib.sha256(f"{user_id}:{experiment}".encode("utf-8")).hexdigest()
    return int(digest, 16) % BUCKETS


# Векторы из sdk/go/synaq_test.go (с ожиданиями, зафиксированными там) —
# проверяем, что сервер до сих пор отвечает так же, и переносим их сюда.
SDK_GROUPS = [
    ("checkout", {"treatment": 50, "control": 50},
     ["control", "control", "control", "treatment", "treatment", "treatment", "control", "control"]),
    ("landing", {"control": 50, "v2": 50},
     ["control", "v2", "v2", "control", "control", "v2", "control", "v2"]),
    ("pricing", {"red": 20, "blue": 30, "control": 50},
     ["control", "blue", "red", "control", "red", "control", "blue", "red"]),
    ("solo", {"only": 100}, ["only"] * 8),
    ("uneven", {"variant_long_name": 7, "b": 3, "control": 13},
     ["variant_long_name", "control", "b", "control", "control", "control", "control", "control"]),
]
SDK_EDGES = [
    ("edge_user_779", "edge_a", {"a": 2, "b": 9, "c": 33}, "c"),
    ("edge_user_483", "edge_b", {"a": 3, "b": 10, "c": 13}, "c"),
    ("edge_user_9603", "edge_c", {"a": 7, "b": 5, "c": 13}, "b"),
    ("edge_user_7707", "half", {"A": 50, "B": 50}, "B"),
    ("edge_user_4656", "half", {"A": 50, "B": 50}, "A"),
    ("edge_user_6057", "last", {"A": 1, "B": 1}, "B"),
    ("edge_user_5788", "first", {"A": 1, "B": 999}, "A"),
]
SDK_HUGE = [("u", "e", "A"), ("big_user_1", "e", "B"), ("big_user_2", "big", "A")]


def vec(user_id, experiment, variants, source, expected=None):
    want = get_user_variant(user_id, experiment, variants)
    if expected is not None and want != expected:
        raise SystemExit(f"сервер разошёлся с sdk/go: {user_id!r} {experiment!r} {variants} -> {want!r}, ждали {expected!r}")
    return {
        "source": source,
        "user_id": user_id,
        "experiment": experiment,
        "variants": variants,
        "bucket": bucket_of(user_id, experiment),
        "want": want,
    }


ALNUM = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-"
UNICODE_POOL = "абвгдеёжзАБВГДЕЁЖЗ日本語中文한국어éüßøñçœ€😀🚀🎉ℕ∑"


def rand_str(rng, alphabet, lo, hi):
    return "".join(rng.choice(alphabet) for _ in range(rng.randint(lo, hi)))


def rand_variants(rng):
    n = rng.randint(1, 20)
    names = set()
    while len(names) < n:
        # Имена вариантов — ASCII (VARIANT_KEY_PATTERN), разной длины, в т.ч. общие префиксы.
        names.add(rand_str(rng, ALNUM, 1, 12))
    mode = rng.random()
    variants = {}
    for name in sorted(names):
        if mode < 0.2:
            variants[name] = 1_000_000
        elif mode < 0.4:
            variants[name] = rng.randint(1, 10)
        else:
            variants[name] = rng.randint(1, 1_000_000)
    return variants


def random_vectors(rng, count):
    out = []
    for i in range(count):
        if i % 3 == 0:
            user_id = rand_str(rng, UNICODE_POOL + ALNUM, 1, 40)
        elif i % 3 == 1:
            user_id = rand_str(rng, ALNUM, 0, 64)
        else:
            user_id = f"user_{rng.randint(0, 10**12)}"
        experiment = rand_str(rng, ALNUM + "абв日", 1, rng.choice([3, 16, 80, 255]))
        out.append(vec(user_id, experiment, rand_variants(rng), "random"))
    return out


def boundary_vectors(rng, count):
    """Пользователи, попадающие ровно на границу (bucket == cumulative*10000/total) и на bucket-1."""
    out = []
    configs = [
        {"a": 1, "b": 1},
        {"a": 1, "b": 3},
        {"a": 1, "b": 1, "c": 2},
        {"a": 2, "b": 9, "c": 33},
        {"x": 1, "y": 4, "z": 5, "w": 10},
        {"a": 250_000, "b": 750_000},
        {"a": 125, "b": 375, "c": 500},
        {"ctl": 3, "t1": 1, "t2": 1, "t3": 3},
        {"a": 999, "b": 1},
        {"a": 1, "b": 9999},
    ]
    for ci, variants in enumerate(configs):
        total = sum(variants.values())
        cumulative = 0
        boundaries = []
        for _, w in sorted(variants.items()):
            cumulative += w
            if (cumulative * BUCKETS) % total == 0:
                b = cumulative * BUCKETS // total
                if b < BUCKETS:
                    boundaries.append(b)
        boundaries.append(0)
        boundaries.append(BUCKETS - 1)
        experiment = f"boundary_{ci}"
        for target in sorted(set(boundaries) | {b - 1 for b in boundaries if b > 0}):
            for n in range(10**7):
                user_id = f"bnd_{ci}_{n}"
                if bucket_of(user_id, experiment) == target:
                    out.append(vec(user_id, experiment, variants, "boundary"))
                    break
            if len(out) >= count:
                return out
    return out


def main():
    rng = random.Random(20261020)
    vectors = []
    for experiment, variants, wants in SDK_GROUPS:
        for i, want in enumerate(wants):
            vectors.append(vec(f"golden_user_{i}", experiment, variants, "sdk_go", want))
    for user_id, experiment, variants, want in SDK_EDGES:
        vectors.append(vec(user_id, experiment, variants, "sdk_go", want))
    huge = {"A": MAX_INT64, "B": MAX_INT64}
    for user_id, experiment, want in SDK_HUGE:
        vectors.append(vec(user_id, experiment, huge, "sdk_go_huge", want))
    vectors.extend(boundary_vectors(rng, 60))
    vectors.extend(random_vectors(rng, 200))

    path = HERE / "golden.json"
    path.write_text(json.dumps(vectors, ensure_ascii=False, indent=1) + "\n", encoding="utf-8")
    print(f"{len(vectors)} векторов -> {path}")


if __name__ == "__main__":
    main()
