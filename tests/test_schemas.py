"""Тесты Pydantic-схем: ExperimentConfigCreate, ExperimentUpdate, VariantResponse.

Валидация:
  - name: 1-50 символов, ^[A-Za-z0-9_.-]+$, не 'active' / 'config'
  - variants: 1-20 вариантов, ключ 1-50 символов ^[A-Za-z0-9_-]+$,
    все веса > 0 (сумма НЕ обязана быть 100 — алгоритм нормализует)
  - config: опциональный Dict[str, Any]
  - url: опциональный, max 200 символов
  - is_active: bool, по умолчанию True
"""

import pytest
from pydantic import ValidationError
from src.schemas.experiment import (
    MAX_VARIANT_WEIGHT,
    ExperimentConfigCreate,
    ExperimentUpdate,
    VariantResponse,
)


# ────────────────────────────────
# ExperimentConfigCreate
# ────────────────────────────────

class TestValidExperiments:
    """Валидные конфигурации — должны проходить."""

    def test_50_50(self):
        exp = ExperimentConfigCreate(name="test_50_50", variants={"A": 50, "B": 50})
        assert exp.name == "test_50_50"
        assert exp.variants == {"A": 50, "B": 50}
        assert exp.is_active is True
        assert exp.config is None
        assert exp.url is None

    def test_33_33_34(self):
        exp = ExperimentConfigCreate(
            name="three_variants",
            variants={"A": 33, "B": 33, "C": 34},
        )
        assert sum(exp.variants.values()) == 100

    def test_with_config(self):
        exp = ExperimentConfigCreate(
            name="with_config",
            variants={"blue": 50, "red": 50},
            config={
                "blue": {"title": "Blue theme"},
                "red": {"title": "Red theme"},
            },
        )
        assert exp.config["blue"]["title"] == "Blue theme"

    def test_with_url(self):
        exp = ExperimentConfigCreate(
            name="url_test",
            variants={"A": 100},
            url="/landing",
        )
        assert exp.url == "/landing"

    def test_single_variant(self):
        exp = ExperimentConfigCreate(name="single", variants={"A": 100})
        assert exp.variants == {"A": 100}

    def test_equal_weights_not_100(self):
        """Веса не обязаны суммироваться в 100 — алгоритм нормализует."""
        exp = ExperimentConfigCreate(name="eq", variants={"A": 33, "B": 33, "C": 33})
        assert sum(exp.variants.values()) == 99  # 33+33+33 = 99, но это ок

    def test_is_active_false(self):
        exp = ExperimentConfigCreate(
            name="disabled_test",
            variants={"A": 50, "B": 50},
            is_active=False,
        )
        assert exp.is_active is False


class TestInvalidExperiments:
    """Невалидные конфигурации — должны кидать ValidationError."""

    def test_negative_weight(self):
        with pytest.raises(ValidationError, match="положительн"):
            ExperimentConfigCreate(name="neg", variants={"A": -10, "B": 110})

    def test_zero_weight(self):
        with pytest.raises(ValidationError, match="положительн"):
            ExperimentConfigCreate(name="zero", variants={"A": 0, "B": 100})

    @pytest.mark.parametrize("name", ["", "a" * 51])
    def test_invalid_name_length(self, name):
        with pytest.raises(ValidationError):
            ExperimentConfigCreate(name=name, variants={"A": 100})

    def test_missing_variants_field(self):
        with pytest.raises(ValidationError):
            ExperimentConfigCreate(name="no_variants")  # type: ignore[call-arg]

    def test_empty_variants_dict(self):
        """Пустой словарь вариантов теперь запрещён (минимум 1 вариант)."""
        with pytest.raises(ValidationError):
            ExperimentConfigCreate(name="empty", variants={})

    @pytest.mark.parametrize("name", [
        "test exp", "<script>alert(1)</script>", "test'exp", "test/exp", "тест", "test;drop", "a\nb",
    ])
    def test_invalid_name_chars(self, name):
        with pytest.raises(ValidationError):
            ExperimentConfigCreate(name=name, variants={"A": 100})

    @pytest.mark.parametrize("name", ["test_exp_1", "test.exp-2", "A", "a" * 50])
    def test_valid_name_chars(self, name):
        assert ExperimentConfigCreate(name=name, variants={"A": 100}).name == name

    @pytest.mark.parametrize("name", ["active", "config", "Active", "CONFIG"])
    def test_reserved_names(self, name):
        with pytest.raises(ValidationError, match="зарезервировано"):
            ExperimentConfigCreate(name=name, variants={"A": 100})

    def test_too_many_variants(self):
        with pytest.raises(ValidationError):
            ExperimentConfigCreate(name="many", variants={f"v{i}": 1 for i in range(21)})

    def test_max_variants_ok(self):
        exp = ExperimentConfigCreate(name="many", variants={f"v{i}": 1 for i in range(20)})
        assert len(exp.variants) == 20

    @pytest.mark.parametrize("key", ["", "a b", "<b>", "A.B", "вариант", "k" * 51])
    def test_invalid_variant_key(self, key):
        with pytest.raises(ValidationError):
            ExperimentConfigCreate(name="bad_key", variants={key: 100})

    @pytest.mark.parametrize("key", ["A", "control", "variant_b-2", "k" * 50])
    def test_valid_variant_key(self, key):
        assert ExperimentConfigCreate(name="good_key", variants={key: 100}).variants == {key: 100}

    def test_all_negative_weights(self):
        with pytest.raises(ValidationError, match="положительн"):
            ExperimentConfigCreate(name="all_neg", variants={"A": -1, "B": -2})

    def test_weight_upper_bound_ok(self):
        exp = ExperimentConfigCreate(name="max_w", variants={"A": 1, "B": MAX_VARIANT_WEIGHT})
        assert exp.variants["B"] == 1_000_000

    @pytest.mark.parametrize("weight", [MAX_VARIANT_WEIGHT + 1, 10**18, 2**63])
    def test_weight_too_large(self, weight):
        with pytest.raises(ValidationError, match="превышать"):
            ExperimentConfigCreate(name="huge_w", variants={"A": 1, "B": weight})


# ────────────────────────────────
# ExperimentUpdate
# ────────────────────────────────

class TestExperimentUpdate:

    @pytest.mark.parametrize("variants", [
        {},
        {f"v{i}": 1 for i in range(21)},
        {"a b": 1},
        {"": 1},
        {"k" * 51: 1},
    ])
    def test_invalid_variants(self, variants):
        with pytest.raises(ValidationError):
            ExperimentUpdate(variants=variants)

    def test_valid_variants(self):
        assert ExperimentUpdate(variants={"A": 1, "B_2": 3}).variants == {"A": 1, "B_2": 3}

    @pytest.mark.parametrize("weight", [0, -1, MAX_VARIANT_WEIGHT + 1])
    def test_weight_out_of_range(self, weight):
        with pytest.raises(ValidationError):
            ExperimentUpdate(variants={"A": weight})

    def test_weight_bounds_ok(self):
        assert ExperimentUpdate(variants={"A": 1, "B": MAX_VARIANT_WEIGHT}).variants["B"] == MAX_VARIANT_WEIGHT


# ────────────────────────────────
# VariantResponse
# ────────────────────────────────

class TestVariantResponse:
    def test_valid_response(self):
        resp = VariantResponse(
            experiment_name="test",
            user_id="user_1",
            variant="B",
        )
        assert resp.experiment_name == "test"
        assert resp.user_id == "user_1"
        assert resp.variant == "B"

    def test_control_variant(self):
        resp = VariantResponse(
            experiment_name="unknown",
            user_id="user_1",
            variant="control",
        )
        assert resp.variant == "control"

    def test_missing_user_id(self):
        with pytest.raises(ValidationError):
            VariantResponse(
                experiment_name="test",
                user_id="",  # пустая строка
                variant="A",
            )

    def test_long_experiment_name(self):
        """VariantResponse не ограничивает длину — проверяем что не падает."""
        resp = VariantResponse(
            experiment_name="x" * 200,
            user_id="user_1",
            variant="A",
        )
        assert len(resp.experiment_name) == 200
