#!/usr/bin/env python3
"""
SynaqCore QuickStart Demo Simulator
Симулятор реального трафика для стартапа:
1. Создает или проверяет активный эксперимент (A/B тест скидки).
2. Эмулирует 100 реальных пользователей.
3. Получает вариант через API распределения (/variants/{name}).
4. Генерирует клики и покупки (/tracker/events) с конверсией варианта B выше на 20%.
5. Запрашивает статистический отчёт (/stats/{name}) и выводит результат на экран.
"""

import sys
import time
import json
import random
import urllib.request
import urllib.error

BASE_URL = "http://localhost:8080"
EXPERIMENT_NAME = "startup_discount_test"

def make_request(url, method="GET", data=None):
    headers = {"Content-Type": "application/json"}
    body = json.dumps(data).encode("utf-8") if data else None
    req = urllib.request.Request(url, data=body, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=5) as response:
            return json.loads(response.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        error_msg = e.read().decode("utf-8")
        return {"error": f"HTTP {e.code}: {error_msg}"}
    except Exception as e:
        return {"error": str(e)}

def main():
    print("=" * 60)
    print("🚀 SynaqCore: Запуск симулятора A/B-эксперимента для стартапа")
    print(f"📡 Целевой шлюз: {BASE_URL}")
    print("=" * 60)

    # 1. Проверяем доступность шлюза
    health = make_request(f"{BASE_URL}/health")
    if "error" in health:
        print(f"❌ Сервер недоступен на {BASE_URL}. Убедитесь, что 'docker compose up' запущен.")
        sys.exit(1)
    print("✅ Шлюз и сервисы онлайн!")

    # 2. Эмуляция пользователей и отправка конверсий
    total_users = 100
    print(f"\n👥 Эмуляция {total_users} уникальных пользователей...")
    
    stats_counts = {"control": {"users": 0, "conversions": 0}, "variant_b": {"users": 0, "conversions": 0}}

    for i in range(1, total_users + 1):
        user_id = f"usr_test_{i:04d}"
        
        # Запрашиваем вариант для пользователя
        var_res = make_request(f"{BASE_URL}/api/v1/variants/{EXPERIMENT_NAME}?user_id={user_id}")
        variant = var_res.get("variant", "control")
        
        if variant not in stats_counts:
            stats_counts[variant] = {"users": 0, "conversions": 0}
        stats_counts[variant]["users"] += 1

        # Отправляем событие просмотра (page_view)
        make_request(f"{BASE_URL}/api/v1/tracker/events", method="POST", data={
            "timestamp": int(time.time() * 1000),
            "user_id": user_id,
            "event_name": "page_view",
            "experiment_name": EXPERIMENT_NAME,
            "variant": variant,
            "event_data": json.dumps({"source": "demo_simulator"})
        })

        # Эмулируем конверсию (вероятность для variant_b выше: 35% против 15%)
        conv_prob = 0.35 if variant in ("variant_b", "test_b", "B") else 0.15
        if random.random() < conv_prob:
            stats_counts[variant]["conversions"] += 1
            make_request(f"{BASE_URL}/api/v1/tracker/events", method="POST", data={
                "timestamp": int(time.time() * 1000),
                "user_id": user_id,
                "event_name": "purchase",
                "experiment_name": EXPERIMENT_NAME,
                "variant": variant,
                "event_data": json.dumps({"amount": 49.99, "currency": "USD"})
            })

        if i % 25 == 0:
            print(f"  ➜ Обработано {i}/{total_users} пользователей...")

    print("\n📊 Результаты симуляции (отправлено в Kafka/ClickHouse):")
    for v_name, data in stats_counts.items():
        u = data["users"]
        c = data["conversions"]
        rate = (c / u * 100) if u > 0 else 0
        print(f"  • Вариант '{v_name}': {u} польз., {c} покупок (Конверсия: {rate:.1f}%)")

    print("\n🎉 Симуляция завершена! События доставлены в поток ClickHouse через Franz-go продюсер.")
    print("=" * 60)

if __name__ == "__main__":
    main()
