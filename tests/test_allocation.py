"""Тесты ядра A/B тестирования — детерминированное распределение (SHA-256).

Алгоритм:
  hash = SHA-256(f"{user_id}:{experiment_name}")
  bucket = int(hash, 16) % 10000           # → 0 .. 9999
  cumulative = 0
  for variant, weight in sorted(variants.items()):   # алфавитный порядок
      cumulative += weight
      if bucket * sum(weights) < cumulative * 10000 → return variant
  return "control"
"""

import hashlib
import itertools

from src.services.allocation import get_user_variant


class TestDeterministic:
    """Один и тот же user_id → всегда один и тот же вариант."""

    def test_same_user_same_variant(self):
        v1 = get_user_variant("user_123", "test_exp", {"A": 50, "B": 50})
        for _ in range(100):
            v2 = get_user_variant("user_123", "test_exp", {"A": 50, "B": 50})
            assert v1 == v2, (
                f"Детерминизм нарушен: первый раз {v1}, потом {v2}"
            )

    def test_different_users_may_differ(self):
        """Разные пользователи — необязательно разные варианты, но проверяем,
        что функция не падает и возвращает допустимые варианты."""
        results = set()
        for i in range(100):
            v = get_user_variant(f"user_{i}", "test_exp", {"A": 50, "B": 50})
            assert v in ("A", "B"), f"Неизвестный вариант: {v}"
            results.add(v)
        # Хотя бы 1 из 2 вариантов встретился (с 100 юзерами — почти гарантированно оба)
        assert len(results) >= 1

    def test_all_three_variants_appear(self):
        """С 1000 пользователями все 3 варианта (33/33/34) должны появиться."""
        results = set()
        for i in range(1000):
            v = get_user_variant(f"user_{i}", "three_test", {"A": 33, "B": 33, "C": 34})
            results.add(v)
        assert results == {"A", "B", "C"}, f"Не все варианты появились: {results}"


class TestDistribution:
    """Статистическая проверка: веса распределения соблюдаются."""

    def test_5050_within_tolerance(self):
        """На 10 000 пользователей разброс не более ±3% (300 юзеров) от 5000."""
        N = 10_000
        a_count = 0
        for i in range(N):
            v = get_user_variant(f"stat_user_{i}", "stat_exp", {"A": 50, "B": 50})
            if v == "A":
                a_count += 1

        expected = N // 2
        tolerance = 300  # 3%
        assert abs(a_count - expected) < tolerance, (
            f"Слишком большой разброс: A={a_count}/{N}, "
            f"ожидалось {expected}±{tolerance}"
        )

    def test_uneven_weights(self):
        """70/30 — грубая проверка: A ≈ 70%."""
        N = 10_000
        a_count = 0
        for i in range(N):
            v = get_user_variant(f"uneven_{i}", "uneven_exp", {"A": 70, "B": 30})
            if v == "A":
                a_count += 1

        ratio = a_count / N
        assert 0.65 < ratio < 0.75, (
            f"Пропорция A={ratio:.3f}, ожидалось ~0.70"
        )


class TestEdgeCases:
    """Граничные случаи."""

    def test_empty_variants(self):
        """Пустой словарь → 'control'."""
        v = get_user_variant("user_1", "exp", {})
        assert v == "control"

    def test_single_variant(self):
        """Один вариант со 100 → он же."""
        v = get_user_variant("user_1", "exp", {"A": 100})
        assert v == "A"

    def test_33_33_33_equal_weights(self):
        """Три равных веса (33,33,33) — все три варианта выдаются."""
        results = set()
        for i in range(2000):
            v = get_user_variant(f"eq_{i}", "eq_exp", {"A": 33, "B": 33, "C": 33})
            results.add(v)
        assert results == {"A", "B", "C"}, f"33/33/33: {results}"

    def test_same_user_different_experiments(self):
        """Один пользователь в разных экспериментах может получить разные варианты."""
        v1 = get_user_variant("user", "exp_1", {"A": 50, "B": 50})
        v2 = get_user_variant("user", "exp_2", {"A": 50, "B": 50})
        # Не обязательно разные, но тест не падает
        assert v1 in ("A", "B")
        assert v2 in ("A", "B")

    def test_weights_not_summing_to_100(self):
        """Веса не обязаны суммироваться в 100 — алгоритм нормализует.
        1+1+1 = 3, каждый получает ~33%."""
        results = set()
        for i in range(2000):
            v = get_user_variant(f"ns_{i}", "ns_exp", {"A": 1, "B": 1, "C": 1})
            results.add(v)
        assert results == {"A", "B", "C"}, f"1+1+1: {results}"


class TestCanonicalOrder:
    """Результат не зависит от порядка ключей в словаре вариантов.

    Конфиг приходит то из Redis (порядок из запроса), то из Postgres JSONB
    (ключи отсортированы по длине, затем по байтам). Варианты перебираются
    в алфавитном порядке, иначе после истечения TTL кэша вариант менялся.
    """

    N = 10_000

    def _assert_order_independent(self, exp: str, variants_a: dict, variants_b: dict):
        assert list(variants_a) != list(variants_b), "порядки должны отличаться"
        for i in range(self.N):
            user_id = f"order_user_{i}"
            assert get_user_variant(user_id, exp, variants_a) == get_user_variant(user_id, exp, variants_b), (
                f"Порядок ключей изменил вариант для {user_id}"
            )

    def test_two_variants_insertion_order(self):
        self._assert_order_independent(
            "order_exp", {"treatment": 50, "control": 50}, {"control": 50, "treatment": 50}
        )

    def test_three_variants_unequal_weights_all_permutations(self):
        weights = {"variant_long": 20, "b": 30, "control": 50}
        base = dict(sorted(weights.items()))
        for perm in itertools.permutations(weights):
            reordered = {k: weights[k] for k in perm}
            if list(reordered) == list(base):
                continue
            self._assert_order_independent("order_three", base, reordered)

    def test_jsonb_order_matches_request_order(self):
        """Порядок «как прислали» и порядок, в котором JSONB отдаёт ключи."""
        request_order = {"treatment": 50, "control": 50}
        jsonb_order = dict(sorted(request_order.items(), key=lambda kv: (len(kv[0]), kv[0].encode())))
        self._assert_order_independent("jsonb_exp", request_order, jsonb_order)

    def test_alphabetical_order_is_canonical(self):
        """Бакет 0 всегда попадает в первый по алфавиту вариант."""
        exp = "canon_exp"
        user_id = next(
            f"u{i}" for i in range(100_000)
            if int(hashlib.sha256(f"u{i}:{exp}".encode()).hexdigest(), 16) % 10000 == 0
        )
        assert get_user_variant(user_id, exp, {"zeta": 50, "alpha": 50}) == "alpha"


class TestExactBoundaries:
    """Сравнение bucket/10000 < cumulative/total выполняется в целых числах."""

    @staticmethod
    def _user_with_bucket(exp: str, bucket: int) -> str:
        i = 0
        while True:
            user_id = f"edge_user_{i}"
            if int(hashlib.sha256(f"{user_id}:{exp}".encode()).hexdigest(), 16) % 10000 == bucket:
                return user_id
            i += 1

    def test_bucket_on_boundary_goes_to_next_variant(self):
        # 50/50: бакет 4999 -> A, ровно 5000 -> B.
        assert get_user_variant(self._user_with_bucket("half", 4999), "half", {"A": 50, "B": 50}) == "A"
        assert get_user_variant(self._user_with_bucket("half", 5000), "half", {"A": 50, "B": 50}) == "B"

    def test_float_rounding_boundary(self):
        # a=2, b=9, c=33 (total 44): бакет 2500 ровно на границе b/c (2500*44 == 11*10000).
        # Float-сравнение в старом SDK относило его к "b".
        user_id = self._user_with_bucket("edge_a", 2500)
        assert get_user_variant(user_id, "edge_a", {"a": 2, "b": 9, "c": 33}) == "c"

    def test_non_positive_weights_fallback(self):
        assert get_user_variant("u", "e", {"A": 0, "B": 100}) == "control"
        assert get_user_variant("u", "e", {"A": -5, "B": 100}) == "control"
