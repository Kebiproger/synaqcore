import hashlib
from typing import Dict

# Число бакетов: bucket = SHA-256(...) % BUCKETS, т.е. шаг 0.01%.
BUCKETS = 10000
DEFAULT_VARIANT = "control"


def get_user_variant(user_id: str, experiment_name: str, variants: Dict[str, int]) -> str:
    """Детерминированно выбирает вариант эксперимента для пользователя.

    bucket = int(SHA-256(f"{user_id}:{experiment_name}"), 16) % 10000 (0..9999).
    Варианты перебираются в каноническом порядке — по имени, по алфавиту
    (Python ``sorted()`` == Go ``sort.Strings``: имена только ASCII,
    см. VARIANT_KEY_PATTERN). Порядок ключей словаря использовать нельзя:
    конфиг приходит то из Redis (порядок из запроса), то из Postgres JSONB,
    который ключи не сохраняет (сортирует по длине, затем по байтам). Без
    сортировки при {"treatment": 50, "control": 50} у всех пользователей
    менялся вариант после истечения TTL кэша.

    Вариант выбирается, если bucket / 10000 < cumulative / total. Сравнение
    в целых числах (bucket * total < cumulative * 10000), без float: на точных
    границах float-округление давало разные ответы на сервере и в SDK.
    Тот же алгоритм обязан совпадать с sdk/python и sdk/go (тесты паритета).
    """
    if not variants or any(w <= 0 for w in variants.values()):
        return DEFAULT_VARIANT

    string_to_hash = f"{user_id}:{experiment_name}"
    hash_hex = hashlib.sha256(string_to_hash.encode("utf-8")).hexdigest()
    bucket = int(hash_hex, 16) % BUCKETS

    total = sum(variants.values())
    cumulative = 0
    for variant_name, weight in sorted(variants.items()):
        cumulative += weight
        if bucket * total < cumulative * BUCKETS:
            return variant_name

    # Недостижимо при положительных весах: последний cumulative == total.
    return DEFAULT_VARIANT
