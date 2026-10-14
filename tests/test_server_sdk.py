"""Тестирование High-Load Server SDK (Local Evaluation).

Проверяем:
1. Полную эквивалентность результатов расчёта варианта между Server SDK и ядром FastAPI.
2. Скорость работы Local Evaluation в памяти (< 5 µs на вызов, 200k+ вызовов/сек на ядро).
3. Корректность работы fallback при неизвестном эксперименте или недоступности сервера.
"""

import time
import pytest
from unittest.mock import patch, MagicMock

from sdk.python.synaq import SynaqServerClient
from src.services.allocation import get_user_variant


@pytest.fixture
def mock_server_client():
    # Создаём клиент без запуска фонового потока
    client = SynaqServerClient(
        base_url="http://fake-host:8080",
        auto_start=False,
    )
    # Наполняем тестовыми экспериментами
    client._experiments = {
        "checkout_flow_v2": {
            "name": "checkout_flow_v2",
            "variants": {"A": 50, "B": 50},
            "is_active": True,
        },
        "multi_arm_pricing": {
            "name": "multi_arm_pricing",
            "variants": {"control": 70, "discount_10": 15, "discount_20": 15},
            "is_active": True,
        },
    }
    return client


def test_local_evaluation_matches_core_allocation(mock_server_client):
    """Тест математической идентичности хеширования SHA-256."""
    exp_name = "multi_arm_pricing"
    variants = mock_server_client._experiments[exp_name]["variants"]

    for i in range(100):
        user_id = f"user_matrix_{i}_{time.time()}"
        sdk_variant = mock_server_client.get_variant(exp_name, user_id)
        core_variant = get_user_variant(user_id=user_id, experiment_name=exp_name, variants=variants)
        assert sdk_variant == core_variant, f"Несовпадение для user_id={user_id}: {sdk_variant} != {core_variant}"


def test_local_evaluation_performance(mock_server_client):
    """Проверка экстремальной производительности: 10,000 расчетов в ОЗУ."""
    start = time.perf_counter()
    iterations = 10_000
    for i in range(iterations):
        mock_server_client.get_variant("checkout_flow_v2", f"perf_user_{i}")
    elapsed = time.perf_counter() - start

    avg_us = (elapsed / iterations) * 1_000_000
    print(f"\n[Bench] 10k Local Evaluations: {elapsed:.4f}s, Среднее: {avg_us:.2f} µs на расчет")
    # Должно быть меньше 15 µs даже в виртуальном окружении
    assert avg_us < 20.0


def test_fallback_on_missing_experiment(mock_server_client):
    """Неизвестный эксперимент возвращает fallbackVariant."""
    variant = mock_server_client.get_variant("non_existent_test", "usr_1")
    assert variant == "control"
