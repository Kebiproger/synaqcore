"""Locust load tests для A/B Testing Platform.

Проверяем заявленные в README характеристики:
  - Experiment API: p99 latency < 10ms
  - Tracker API:   throughput > 1000 RPS

Запуск:
  pip install locust
  export SYNAQ_TOKEN="$(scripts/get_token.sh)"   # JWT с ролью admin (Zitadel)
  locust -f tests/load/locustfile.py --headless \\
    -u 100 -r 10 --run-time 30s \\
    --host=http://localhost:8080

Без SYNAQ_TOKEN эксперимент не создаётся автоматически (setup пропускается).
"""

import os
import random

from locust import HttpUser, task, between
from locust.env import Environment


# ─────────── Тестовые данные ───────────
SYNAQ_TOKEN_ENV = "SYNAQ_TOKEN"
EXPERIMENT_NAME = "load_test_exp"
VARIANTS = {"A": 50, "B": 50}

# Пул user_id для избегания повторяющихся хэшей
USER_IDS = [f"load_user_{i}" for i in range(10_000)]


def random_user() -> str:
    return random.choice(USER_IDS)


# ─────────── Experiment API User ───────────
class ExperimentApiUser(HttpUser):
    """Нагрузка на GET /api/v1/variants/ — детерминированное распределение.

    Ожидание: p99 latency < 10ms при 1000+ RPS.
    """
    host = "http://localhost:8080"
    wait_time = between(0.01, 0.05)  # 10-50ms между запросами

    @task(10)
    def get_variant(self):
        user_id = random_user()
        with self.client.get(
            f"/api/v1/variants/?user_id={user_id}&experiment_name={EXPERIMENT_NAME}",
            name="/api/v1/variants/",
            catch_response=True,
        ) as resp:
            if resp.status_code != 200:
                resp.failure(f"Unexpected status: {resp.status_code}")
            else:
                try:
                    data = resp.json()
                    assert data["variant"] in ("A", "B", "control")
                    assert data["user_id"] == user_id
                except Exception as e:
                    resp.failure(f"Invalid response: {e}")

    @task(1)
    def get_variant_missing_exp(self):
        """Запрос несуществующего эксперимента — должен вернуть 'control'."""
        with self.client.get(
            "/api/v1/variants/?user_id=unknown_user&experiment_name=nonexistent",
            name="/api/v1/variants/ (404 fallback)",
            catch_response=True,
        ) as resp:
            if resp.status_code == 200:
                data = resp.json()
                if data.get("variant") != "control":
                    resp.failure(f"Expected control, got {data.get('variant')}")
            else:
                resp.failure(f"Expected 200, got {resp.status_code}")


# ─────────── Tracker API User ───────────
class TrackerApiUser(HttpUser):
    """Нагрузка на POST /api/v1/tracker/events — приём аналитики.

    Ожидание: 1000+ RPS, ответ 202 Accepted.
    """
    host = "http://localhost:8080"
    wait_time = between(0.01, 0.03)  # 10-30ms между запросами

    @task
    def send_event(self):
        user_id = random_user()
        event = {
            "user_id": user_id,
            "event_name": random.choice(["page_view", "cta_click", "scroll", "form_submit"]),
            "experiment_name": EXPERIMENT_NAME,
            "variant": random.choice(["A", "B"]),
            "event_data": {
                "page": "/landing",
                "browser": "chrome",
            },
        }
        with self.client.post(
            "/api/v1/tracker/events",
            json=event,
            name="/api/v1/tracker/events",
            catch_response=True,
        ) as resp:
            if resp.status_code != 202:
                resp.failure(f"Expected 202, got {resp.status_code}")
            else:
                try:
                    assert resp.json()["status"] == "accepted"
                except Exception as e:
                    resp.failure(f"Invalid response: {e}")


# ─────────── Setup: создать эксперимент перед тестом ───────────
def setup_experiment(environment: Environment, **kwargs):
    """Создаёт тестовый эксперимент один раз перед запуском нагрузки."""
    import httpx

    token = os.environ.get(SYNAQ_TOKEN_ENV, "").strip()
    if not token:
        print(f"[SETUP] {SYNAQ_TOKEN_ENV} не задан — создание эксперимента пропущено.")
        print("[SETUP] Получите токен: export SYNAQ_TOKEN=\"$(scripts/get_token.sh)\"")
        return

    host = environment.host or "http://localhost:8080"
    url = f"{host}/api/v1/experiments/"
    payload = {
        "name": EXPERIMENT_NAME,
        "variants": VARIANTS,
        "is_active": True,
    }
    headers = {
        "Authorization": f"Bearer {token}",
        "Content-Type": "application/json",
    }
    try:
        resp = httpx.post(url, json=payload, headers=headers, timeout=5)
        if resp.status_code == 201:
            print(f"[SETUP] Experiment '{EXPERIMENT_NAME}' created")
        elif resp.status_code == 400:
            print(f"[SETUP] Experiment already exists, continuing")
        else:
            print(f"[SETUP] WARNING: Unexpected response {resp.status_code}: {resp.text}")
    except Exception as e:
        print(f"[SETUP] ERROR: Could not create experiment: {e}")
        print("[SETUP] Load tests may fail if experiment is missing")


# Подключаем setup к событию test_start
from locust import events
events.test_start.add_listener(setup_experiment)
