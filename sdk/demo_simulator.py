#!/usr/bin/env python3
"""
SynaqCore Demo Simulator — сквозная демонстрация A/B-теста через шлюз.

Что делает:
  1. Проверяет доступность шлюза (/health).
  2. Идемпотентно создаёт эксперимент (control/treatment 50/50, активный)
     через Admin API (нужен JWT с ролью admin в переменной SYNAQ_TOKEN).
  3. Распределяет синтетических пользователей по вариантам. По умолчанию —
     локально, через серверный SDK (sdk/python, /variants/eval-configs) с
     выборочной сверкой с сервером; это обходит лимит nginx на /api/v1/variants/
     (50 r/s на IP, burst 100). Режим --allocation remote спрашивает сервер
     для каждого пользователя с ограничением частоты и повтором при 429.
  4. Отправляет события воронки (page_view → add_to_cart → checkout_start →
     purchase) в /api/v1/tracker/events с уникальными event_id, конкурентно.
  5. Ждёт, пока события дойдут до ClickHouse (Kafka → MV, асинхронно), и
     печатает отчёт /api/v1/stats/<experiment>: конверсии, uplift, p-value,
     вероятность превосходства, рекомендации.

Токен отправляется только в /api/v1/experiments и /api/v1/stats. По открытому
http:// он уходит только на localhost; для другого хоста нужен https:// или
явный флаг --insecure. После MAX_CONSECUTIVE_FAILURES ошибок трекера подряд
отправка прерывается. Нужен Python 3.11+ (asyncio.TaskGroup).

Все данные синтетические (demo_<run>_<n>), реальных пользователей нет.

Пример:
    export SYNAQ_TOKEN="$(scripts/get_token.sh)"
    python3 sdk/demo_simulator.py --users 5000 --uplift 0.15 --seed 42
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import random
import sys
import time
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Awaitable, Iterable, Optional, TypeVar
from urllib.parse import urlsplit

try:
    import httpx
except ImportError:  # pragma: no cover - проверяется в main()
    httpx = None  # type: ignore[assignment]

CONTROL = "control"
TREATMENT = "treatment"
PLATFORMS = ("web", "ios", "android")
TENURES = ("new", "returning")
FUNNEL = ("page_view", "add_to_cart", "checkout_start")

# Лимит nginx на /api/v1/variants/: 50 r/s, burst 100. Держимся ниже.
DEFAULT_VARIANT_RPS = 40.0
VERIFY_SAMPLE = 20
MAX_RETRIES = 6
# Повторы при 5xx/сетевых ошибках трекера: не больше двух на событие
TRACKER_SERVER_RETRIES = 2
# Retry-After от сервера не может заставить ждать дольше этого
RETRY_AFTER_CAP = 10.0
# Столько неудачных событий подряд — и отправка прерывается (fail fast)
MAX_CONSECUTIVE_FAILURES = 20
# Хосты, на которые токен можно слать по открытому http://
LOCAL_HOSTS = frozenset({"localhost", "127.0.0.1", "::1"})

T = TypeVar("T")


class DemoError(Exception):
    """Ошибка, после которой продолжать демо бессмысленно."""


# ──────────────────────────────────────────────
#  Аргументы командной строки
# ──────────────────────────────────────────────

def parse_args(argv: Optional[list[str]] = None) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description="Симулятор A/B-эксперимента SynaqCore: создаёт эксперимент, "
                    "генерирует синтетический трафик и печатает статистический отчёт.",
        epilog="Токен берётся из переменной окружения SYNAQ_TOKEN "
               "(получить: eval \"$(make -s token)\" или export SYNAQ_TOKEN=\"$(scripts/get_token.sh)\").",
    )
    p.add_argument("--host", default="http://localhost:8080", help="адрес шлюза (по умолчанию %(default)s)")
    p.add_argument("--experiment", default="demo_checkout_button", help="имя эксперимента (по умолчанию %(default)s)")
    p.add_argument("--users", type=int, default=5000, help="число синтетических пользователей (по умолчанию %(default)s)")
    p.add_argument("--uplift", type=float, default=0.15,
                   help="относительный прирост конверсии treatment к control (по умолчанию %(default)s = +15%%)")
    p.add_argument("--base-conversion", type=float, default=0.10,
                   help="конверсия control в покупку (по умолчанию %(default)s)")
    p.add_argument("--goal-event", default="purchase", help="целевое событие (по умолчанию %(default)s)")
    p.add_argument("--seed", type=int, default=None, help="seed генератора для воспроизводимых конверсий")
    p.add_argument("--allocation", choices=("local", "remote"), default="local",
                   help="local — расчёт в памяти через sdk/python (без лимита nginx); "
                        "remote — запрос к /api/v1/variants/ на каждого пользователя (по умолчанию %(default)s)")
    p.add_argument("--variant-rps", type=float, default=DEFAULT_VARIANT_RPS,
                   help="предел запросов/с к /api/v1/variants/ (лимит nginx 50 r/s; по умолчанию %(default)s)")
    p.add_argument("--concurrency", type=int, default=50,
                   help="число одновременных запросов к трекеру (по умолчанию %(default)s)")
    p.add_argument("--stats-timeout", type=float, default=60.0,
                   help="сколько секунд ждать появления данных в /stats (по умолчанию %(default)s)")
    p.add_argument("--insecure", action="store_true",
                   help="разрешить http:// к хосту не на localhost (токен уйдёт открытым текстом)")
    args = p.parse_args(argv)

    url = urlsplit(args.host)
    if url.scheme not in ("http", "https") or not url.hostname:
        p.error("--host должен быть URL вида http(s)://host[:port]")
    if url.scheme == "http" and url.hostname not in LOCAL_HOSTS and not args.insecure:
        p.error(f"--host {args.host}: по http:// токен ушёл бы открытым текстом. "
                "Используйте https:// или явно укажите --insecure")

    if args.users < 1:
        p.error("--users должно быть >= 1")
    if not 0 < args.base_conversion < 1:
        p.error("--base-conversion должно быть в интервале (0, 1)")
    if args.uplift <= -1:
        p.error("--uplift должно быть > -1")
    if not 0 < args.base_conversion * (1 + args.uplift) <= 1:
        p.error("конверсия treatment = base-conversion * (1 + uplift) должна быть в (0, 1]")
    if args.variant_rps <= 0 or args.concurrency < 1:
        p.error("--variant-rps и --concurrency должны быть положительными")
    return args


# ──────────────────────────────────────────────
#  HTTP-утилиты
# ──────────────────────────────────────────────

class RateLimiter:
    """Простой асинхронный ограничитель: не чаще rps запросов в секунду."""

    def __init__(self, rps: float):
        self._interval = 1.0 / rps
        self._next = 0.0
        self._lock = asyncio.Lock()

    async def wait(self) -> None:
        async with self._lock:
            now = time.monotonic()
            delay = self._next - now
            self._next = max(now, self._next) + self._interval
        if delay > 0:
            await asyncio.sleep(delay)


def _retry_delay(resp: Optional["httpx.Response"], attempt: int) -> float:
    if resp is not None:
        retry_after = resp.headers.get("Retry-After")
        if retry_after and retry_after.isdigit():
            return min(float(retry_after), RETRY_AFTER_CAP)
    return min(0.5 * 2 ** attempt, 8.0)


async def request_with_retry(
    client: "httpx.AsyncClient",
    method: str,
    url: str,
    *,
    limiter: Optional[RateLimiter] = None,
    max_server_retries: Optional[int] = None,
    **kwargs: Any,
) -> "httpx.Response":
    """Запрос с повтором при 429, 5xx и сетевых ошибках (экспоненциальная пауза).

    max_server_retries ограничивает повторы именно при 5xx/сетевых ошибках
    (429 по-прежнему повторяется до MAX_RETRIES попыток).
    """
    last_exc: Optional[Exception] = None
    server_failures = 0
    for attempt in range(MAX_RETRIES):
        if limiter is not None:
            await limiter.wait()
        resp = None
        try:
            resp = await client.request(method, url, **kwargs)
            if resp.status_code != 429 and resp.status_code < 500:
                return resp
            if resp.status_code >= 500:
                server_failures += 1
        except httpx.TransportError as exc:
            last_exc = exc
            server_failures += 1
        if max_server_retries is not None and server_failures > max_server_retries:
            break
        await asyncio.sleep(_retry_delay(resp, attempt))
    if resp is not None:
        return resp
    raise DemoError(f"{method} {url}: сеть недоступна ({type(last_exc).__name__}: {last_exc})")


def _first_error(group: BaseExceptionGroup) -> BaseException:
    """Первая «листовая» ошибка группы; DemoError — в приоритете."""
    demo = group.subgroup(DemoError)
    exc: BaseException = demo if demo is not None else group
    while isinstance(exc, BaseExceptionGroup):
        exc = exc.exceptions[0]
    return exc


async def run_all(coros: Iterable[Awaitable[T]]) -> list[T]:
    """Запустить корутины в asyncio.TaskGroup и вернуть результаты по порядку.

    Первая ошибка отменяет все остальные задачи; наружу выходит сама ошибка
    (а не ExceptionGroup), чтобы main() показал понятное сообщение.
    """
    try:
        async with asyncio.TaskGroup() as tg:
            tasks = [tg.create_task(c) for c in coros]
    except BaseExceptionGroup as group:
        raise _first_error(group) from None
    return [t.result() for t in tasks]


class FailureBudget:
    """Общий для всех воркеров счётчик ошибок подряд."""

    def __init__(self, limit: int):
        self.limit = limit
        self.consecutive = 0

    def success(self) -> None:
        self.consecutive = 0

    def failure(self) -> bool:
        """Учесть ошибку; True — лимит ошибок подряд исчерпан."""
        self.consecutive += 1
        return self.consecutive >= self.limit


def _detail(resp: "httpx.Response") -> str:
    try:
        body = resp.json()
        if isinstance(body, dict) and "detail" in body:
            return str(body["detail"])
        return json.dumps(body, ensure_ascii=False)[:300]
    except ValueError:
        return resp.text[:300]


def _check_auth(resp: "httpx.Response", what: str) -> None:
    if resp.status_code == 401:
        raise DemoError(f"{what}: 401 — токен недействителен или истёк. Получите новый: eval \"$(make -s token)\"")
    if resp.status_code == 403:
        raise DemoError(f"{what}: 403 — у токена нет нужной роли (нужна admin/analyst)")
    if resp.status_code == 503:
        raise DemoError(f"{what}: 503 — авторизация не настроена (OIDC/JWKS недоступны): {_detail(resp)}")


# ──────────────────────────────────────────────
#  Шаги демо
# ──────────────────────────────────────────────

async def check_health(client: "httpx.AsyncClient", host: str) -> None:
    try:
        resp = await client.get(f"{host}/health", timeout=5)
    except httpx.TransportError as exc:
        raise DemoError(f"Шлюз {host} недоступен ({type(exc).__name__}). Запустите стек: make up") from exc
    if resp.status_code != 200:
        raise DemoError(f"Шлюз {host} ответил {resp.status_code} на /health: {_detail(resp)}")


async def ensure_experiment(client: "httpx.AsyncClient", host: str, name: str,
                            auth: dict[str, str]) -> None:
    """Создать эксперимент, если его нет; включить, если выключен. Повторный запуск безопасен."""
    url = f"{host}/api/v1/experiments/"
    resp = await request_with_retry(client, "GET", url, headers=auth)
    _check_auth(resp, "Список экспериментов")
    if resp.status_code != 200:
        raise DemoError(f"Не удалось получить список экспериментов: {resp.status_code} {_detail(resp)}")

    existing = next((e for e in resp.json() if e.get("name") == name), None)
    wanted = {CONTROL: 50, TREATMENT: 50}

    if existing is None:
        payload = {
            "name": name,
            "variants": wanted,
            "config": {CONTROL: {"button_color": "grey"}, TREATMENT: {"button_color": "green"}},
            "is_active": True,
        }
        resp = await request_with_retry(client, "POST", url, json=payload, headers=auth)
        _check_auth(resp, "Создание эксперимента")
        if resp.status_code == 201:
            print(f"  Эксперимент '{name}' создан: {CONTROL}/{TREATMENT} 50/50")
            return
        if resp.status_code == 400 and "уже существует" in _detail(resp):
            print(f"  Эксперимент '{name}' уже создан параллельно — используем его")
            return
        raise DemoError(f"Не удалось создать эксперимент: {resp.status_code} {_detail(resp)}")

    variants = existing.get("variants") or {}
    if set(variants) != set(wanted):
        raise DemoError(
            f"Эксперимент '{name}' уже существует с вариантами {sorted(variants)}, "
            f"а демо ожидает {sorted(wanted)}. Укажите другое имя: --experiment <name>"
        )
    if not existing.get("is_active"):
        resp = await request_with_retry(client, "PATCH", f"{host}/api/v1/experiments/{name}",
                                        json={"is_active": True}, headers=auth)
        _check_auth(resp, "Включение эксперимента")
        if resp.status_code != 200:
            raise DemoError(f"Не удалось включить эксперимент: {resp.status_code} {_detail(resp)}")
        print(f"  Эксперимент '{name}' был выключен — включён")
    else:
        print(f"  Эксперимент '{name}' уже существует и активен (веса {variants})")


async def remote_variant(client: "httpx.AsyncClient", host: str, name: str,
                         user_id: str, limiter: RateLimiter) -> str:
    resp = await request_with_retry(client, "GET", f"{host}/api/v1/variants/{name}",
                                    params={"user_id": user_id}, limiter=limiter)
    if resp.status_code != 200:
        raise DemoError(f"/api/v1/variants/{name}: {resp.status_code} {_detail(resp)}")
    return resp.json()["variant"]


async def eval_config_names(client: "httpx.AsyncClient", host: str) -> Optional[set[str]]:
    """Имена экспериментов из /api/v1/variants/eval-configs (публичный эндпоинт SDK).

    У sdk/python нет публичного метода «есть ли эксперимент», поэтому проверяем
    сами, не залезая в приватное состояние клиента. None — ответ непригоден.
    """
    resp = await request_with_retry(client, "GET", f"{host}/api/v1/variants/eval-configs")
    if resp.status_code != 200:
        return None
    try:
        experiments = resp.json().get("experiments")
    except (ValueError, AttributeError):
        return None
    return set(experiments) if isinstance(experiments, dict) else None


def load_local_allocator(host: str):
    """Серверный SDK (sdk/python) с локальным расчётом варианта. None — если недоступен."""
    sdk_path = Path(__file__).resolve().parent / "python"
    if str(sdk_path) not in sys.path:
        sys.path.insert(0, str(sdk_path))
    try:
        from synaq import SynaqServerClient
    except ImportError as exc:
        print(f"  [!] sdk/python не импортируется ({exc}), переход на remote")
        return None
    return SynaqServerClient(base_url=host, auto_start=False, timeout=5.0)


async def allocate(client: "httpx.AsyncClient", args: argparse.Namespace,
                   user_ids: list[str]) -> tuple[dict[str, str], str]:
    """Вернуть {user_id: variant} и фактически использованный режим."""
    host, name = args.host, args.experiment
    limiter = RateLimiter(args.variant_rps)
    mode = args.allocation

    if mode == "local":
        sdk = None
        names = await eval_config_names(client, host)
        if names is None or name not in names:
            print("  [!] Эксперимента нет в /api/v1/variants/eval-configs, переход на remote")
        else:
            sdk = await asyncio.to_thread(load_local_allocator, host)
        if sdk is not None:
            # Сверяем выборку с сервером: локальный расчёт должен совпадать с API
            # (заодно ловит случай, когда SDK не смог загрузить конфиги)
            sample = user_ids[:VERIFY_SAMPLE]
            server = await run_all(remote_variant(client, host, name, u, limiter) for u in sample)
            mismatches = sum(sdk.get_variant(name, u) != v for u, v in zip(sample, server))
            if mismatches == 0:
                print(f"  Локальный расчёт (sdk/python) совпал с сервером на {len(sample)} пользователях")
                return {u: sdk.get_variant(name, u) for u in user_ids}, "local"
            print(f"  [!] Локальный расчёт расходится с сервером ({mismatches}/{len(sample)}), переход на remote")
        mode = "remote"

    eta = len(user_ids) / args.variant_rps
    print(f"  Запрашиваем варианты у сервера: {len(user_ids)} запросов при {args.variant_rps:g} r/s (~{eta:.0f} с)")
    result: dict[str, str] = {}
    sem = asyncio.Semaphore(args.concurrency)
    done = 0

    async def one(uid: str) -> None:
        nonlocal done
        async with sem:
            result[uid] = await remote_variant(client, host, name, uid, limiter)
        done += 1
        if done % 500 == 0 or done == len(user_ids):
            print(f"    варианты: {done}/{len(user_ids)}")

    await run_all(one(u) for u in user_ids)
    return result, mode


@dataclass
class UserPlan:
    user_id: str
    variant: str
    events: list[dict] = field(default_factory=list)
    converted: bool = False


def build_plans(args: argparse.Namespace, user_ids: list[str], variants: dict[str, str],
                rng: random.Random) -> list[UserPlan]:
    """Сгенерировать события для каждого пользователя (детерминированно при --seed)."""
    p_control = args.base_conversion
    p_treatment = args.base_conversion * (1 + args.uplift)
    now_ms = int(time.time() * 1000)
    plans: list[UserPlan] = []

    for uid in user_ids:
        variant = variants[uid]
        p_conv = p_treatment if variant == TREATMENT else p_control
        converted = rng.random() < p_conv
        profile = {"platform": rng.choice(PLATFORMS), "user_tenure": rng.choice(TENURES),
                   "source": "demo_simulator"}

        # Не конвертировавшиеся пользователи частично проходят воронку
        if converted:
            steps = list(FUNNEL)
        else:
            steps = ["page_view"]
            if rng.random() < 0.35:
                steps.append("add_to_cart")
                if rng.random() < 0.3:
                    steps.append("checkout_start")
        if converted:
            steps.append(args.goal_event)

        plan = UserPlan(user_id=uid, variant=variant, converted=converted)
        ts = now_ms - rng.randint(0, 600_000)
        for step in steps:
            data = dict(profile)
            if step == "purchase":
                data["revenue"] = round(rng.uniform(20.0, 120.0), 2)
                data["currency"] = "USD"
            plan.events.append({
                "event_id": str(uuid.UUID(int=rng.getrandbits(128), version=4)),
                "user_id": uid,
                "event_name": step,
                "timestamp": ts,
                "experiment_name": args.experiment,
                "variant": variant,
                # Трекер и SDK передают event_data строкой JSON (колонка String в ClickHouse)
                "event_data": json.dumps(data, ensure_ascii=False),
            })
            ts += rng.randint(1_000, 30_000)
        plans.append(plan)
    return plans


async def send_events(client: "httpx.AsyncClient", args: argparse.Namespace,
                      plans: list[UserPlan]) -> tuple[int, int]:
    """Отправить события в трекер. Возвращает (успешно, с ошибкой).

    После MAX_CONSECUTIVE_FAILURES ошибок подряд (общий счётчик всех воркеров)
    поднимается DemoError, и TaskGroup отменяет оставшуюся отправку.
    """
    url = f"{args.host}/api/v1/tracker/events"
    sem = asyncio.Semaphore(args.concurrency)
    total = sum(len(p.events) for p in plans)
    ok = failed = 0
    started = time.monotonic()
    step = max(1, total // 10)
    budget = FailureBudget(MAX_CONSECUTIVE_FAILURES)

    async def one(event: dict) -> None:
        nonlocal ok, failed
        async with sem:
            try:
                resp = await request_with_retry(client, "POST", url, json=event,
                                                max_server_retries=TRACKER_SERVER_RETRIES)
                success = resp.status_code == 200
                reason = f"HTTP {resp.status_code} {_detail(resp)}"
            except DemoError as exc:
                success = False
                reason = str(exc)
        if success:
            ok += 1
            budget.success()
        else:
            failed += 1
            if budget.failure():
                raise DemoError(
                    f"Трекер: {budget.limit} ошибок подряд (последняя: {reason}). "
                    f"Отправка прервана: принято {ok}, ошибок {failed} из {total}"
                )
        sent = ok + failed
        if sent % step == 0 or sent == total:
            rate = sent / max(time.monotonic() - started, 1e-6)
            print(f"    события: {sent}/{total} ({rate:.0f}/с, ошибок {failed})")

    # Пользователь отправляет события по порядку воронки; пользователи — параллельно
    async def user(plan: UserPlan) -> None:
        for event in plan.events:
            await one(event)

    await run_all(user(p) for p in plans)
    return ok, failed


async def fetch_stats(client: "httpx.AsyncClient", args: argparse.Namespace,
                      auth: dict[str, str], *, transient_ok: bool = False) -> tuple[int, Optional[dict]]:
    """(статус, отчёт). 400/404 — данных нет или вариант один; 5xx — временный сбой.

    transient_ok=True (цикл ожидания): любой 5xx, включая 503, возвращается
    вызывающему для повтора, а не превращается в DemoError.
    """
    resp = await request_with_retry(
        client, "GET", f"{args.host}/api/v1/stats/{args.experiment}",
        params={"goal_event": args.goal_event, "method": "both"}, timeout=30, headers=auth,
    )
    if transient_ok and resp.status_code >= 500:
        return resp.status_code, None
    _check_auth(resp, "Статистика")
    if resp.status_code == 200:
        return 200, resp.json()
    if resp.status_code in (400, 404) or resp.status_code >= 500:
        return resp.status_code, None
    raise DemoError(f"/api/v1/stats: {resp.status_code} {_detail(resp)}")


async def fetch_basic_users(client: "httpx.AsyncClient", args: argparse.Namespace,
                            auth: dict[str, str]) -> Optional[int]:
    """Число пользователей по /stats/<exp>/basic (работает и при одном варианте). None — неизвестно."""
    resp = await request_with_retry(
        client, "GET", f"{args.host}/api/v1/stats/{args.experiment}/basic", timeout=30, headers=auth,
    )
    _check_auth(resp, "Базовая статистика")
    if resp.status_code != 200:
        return None
    try:
        summary = resp.json().get("summary")
        return sum(int(r.get("unique_users", 0)) for r in summary)
    except (ValueError, TypeError, AttributeError):
        return None


async def fetch_baseline(client: "httpx.AsyncClient", args: argparse.Namespace,
                         auth: dict[str, str]) -> Optional[int]:
    """Сколько пользователей уже было в эксперименте до запуска. None — неизвестно."""
    status, before = await fetch_stats(client, args, auth)
    if status == 200:
        return _total_users(before)
    if status == 404:
        return 0  # данных по эксперименту нет вовсе
    # 400 (например, данные только по одному варианту) или 5xx: полный отчёт
    # не говорит, сколько данных уже есть — пробуем /basic
    return await fetch_basic_users(client, args, auth)


def _total_users(stats: Optional[dict]) -> int:
    if not stats:
        return 0
    return sum(int(v.get("total_users", 0)) for v in stats.get("variants", {}).values())


async def wait_for_stats(client: "httpx.AsyncClient", args: argparse.Namespace,
                         auth: dict[str, str], baseline: Optional[int]) -> Optional[dict]:
    """Ждать, пока ClickHouse догонит Kafka (асинхронная загрузка).

    baseline=None (объём старых данных неизвестен): ждём хотя бы args.users
    пользователей и, чтобы не принять старые данные за новые, ещё и того, что
    счётчик перестал расти между двумя опросами.
    """
    expected_users = args.users if baseline is None else baseline + args.users
    deadline = time.monotonic() + args.stats_timeout
    last: Optional[dict] = None
    previous = -1
    while True:
        status, stats = await fetch_stats(client, args, auth, transient_ok=True)
        if stats is not None:
            last = stats
        elif status >= 500:
            print(f"    /stats временно недоступен (HTTP {status}), повторяю")
        seen = _total_users(last)
        print(f"    в ClickHouse пользователей: {seen}/{expected_users}"
              + (" (базовый объём неизвестен)" if baseline is None else ""))
        if seen >= expected_users and (baseline is not None or seen == previous):
            return last
        previous = seen
        if time.monotonic() >= deadline:
            print(f"  [!] За {args.stats_timeout:g} с дошли не все события — показываю то, что есть")
            return last
        await asyncio.sleep(3)


# ──────────────────────────────────────────────
#  Вывод отчёта
# ──────────────────────────────────────────────

def _pct(x: Optional[float], digits: int = 2) -> str:
    return "—" if x is None else f"{x * 100:.{digits}f}%"


def _num(x: Any, fmt: str = ".4f") -> str:
    return "—" if x is None else format(x, fmt)


def print_report(stats: dict, local: dict[str, dict[str, int]]) -> None:
    line = "=" * 72
    print(f"\n{line}\nОтчёт /api/v1/stats/{stats.get('experiment')} "
          f"(цель: {stats.get('goal_event')}, метод: {stats.get('method')})\n{line}")

    print(f"{'Вариант':<12}{'Польз.':>9}{'Конверсий':>11}{'Конверсия':>11}   | отправлено: польз./конв.")
    for name, v in sorted(stats.get("variants", {}).items()):
        sent = local.get(name, {"users": 0, "conversions": 0})
        print(f"{name:<12}{v.get('total_users', 0):>9}{v.get('converting_users', 0):>11}"
              f"{_pct(v.get('conversion_rate')):>11}   | {sent['users']}/{sent['conversions']}")

    st = stats.get("statistical") or {}
    if "error" in st:
        print(f"\nСтатистика: не посчитана ({st['error']})")
    else:
        rates = st.get("sample_rates") or {}
        freq = st.get("frequentist") or {}
        bayes = st.get("bayesian") or {}
        lift = rates.get("relative_lift_pct")
        print("\nСтатистика")
        print(f"  Относительный uplift:       {'—' if lift is None else f'{lift:+.2f}%'}")
        print(f"  Абсолютная разница:         {_pct(rates.get('absolute_difference'), 3)}")
        ci = freq.get("ci_relative_lift")
        if ci:
            print(f"  95% ДИ uplift:              [{ci[0] * 100:+.2f}%, {ci[1] * 100:+.2f}%]")
        if freq:
            sig = freq.get("is_significant")
            print(f"  p-value (z-тест):           {_num(freq.get('p_value'), '.6f')}"
                  f"  ({'значимо' if sig else 'не значимо'}, уровень {freq.get('confidence_level')})")
        if bayes:
            print(f"  P(treatment > control):     {_pct(bayes.get('probability_b_better'))}")
            print(f"  Ожидаемые потери (выбор B): {_num(bayes.get('expected_loss_choose_b'), '.6f')}")

    revenue = (stats.get("business") or {}).get("revenue") or {}
    arpu = revenue.get("arpu") or {}
    if arpu:
        rel = arpu.get("uplift_relative_pct")
        print(f"\nВыручка: ARPU treatment {_num(arpu.get('variant'), '.2f')} / control "
              f"{_num(arpu.get('control'), '.2f')} USD, uplift {'—' if rel is None else f'{rel:+.2f}%'}")

    recs = stats.get("recommendations") or []
    if recs:
        print("\nРекомендации")
        for r in recs:
            conf = f" [уверенность {r['confidence']}]" if r.get("confidence") else ""
            print(f"  * {r.get('summary') or ''}{conf}")
            if r.get("action"):
                print(f"    Действие: {r['action']}")
            if r.get("risk_note"):
                print(f"    Риск: {r['risk_note']}")
    print(line)


# ──────────────────────────────────────────────
#  Точка входа
# ──────────────────────────────────────────────

async def run(args: argparse.Namespace, token: str) -> int:
    rng = random.Random(args.seed)
    # Уникальный префикс запуска: повторные запуски добавляют новых пользователей,
    # а не перезаписывают старых (event_id тоже уникальны)
    run_id = uuid.uuid4().hex[:8]
    user_ids = [f"demo_{run_id}_{i:05d}" for i in range(1, args.users + 1)]

    print("=" * 72)
    print(f"SynaqCore demo: эксперимент '{args.experiment}', {args.users} пользователей, "
          f"control {args.base_conversion:.1%} → treatment {args.base_conversion * (1 + args.uplift):.1%}")
    print(f"Шлюз: {args.host}, запуск {run_id}, seed {args.seed}")
    print("=" * 72)

    limits = httpx.Limits(max_connections=args.concurrency + 10, max_keepalive_connections=args.concurrency)
    # Токен — только в запросы к /api/v1/experiments и /api/v1/stats; трекер,
    # /variants и /health его не получают
    auth = {"Authorization": f"Bearer {token}"}
    headers = {"User-Agent": "SynaqDemoSimulator/2.0"}
    async with httpx.AsyncClient(headers=headers, timeout=10, limits=limits) as client:
        print("\n[1/5] Проверка шлюза")
        await check_health(client, args.host)
        print("  Шлюз отвечает")

        print("\n[2/5] Эксперимент")
        await ensure_experiment(client, args.host, args.experiment, auth)
        baseline = await fetch_baseline(client, args, auth)
        if baseline is None:
            print("  [!] Не удалось узнать объём уже накопленных данных — "
                  "буду ждать, пока счётчик в /stats перестанет расти")
        elif baseline:
            print(f"  В эксперименте уже есть данные: {baseline} пользователей (добавим новых)")

        print(f"\n[3/5] Распределение по вариантам (режим {args.allocation})")
        variants, used_mode = await allocate(client, args, user_ids)
        if used_mode != args.allocation:
            print(f"  Использован режим: {used_mode}")

        plans = build_plans(args, user_ids, variants, rng)
        local: dict[str, dict[str, int]] = {}
        for p in plans:
            s = local.setdefault(p.variant, {"users": 0, "conversions": 0})
            s["users"] += 1
            s["conversions"] += int(p.converted)
        split = ", ".join(f"{k}: {v['users']}" for k, v in sorted(local.items()))
        print(f"  Распределение: {split}")

        print("\n[4/5] Отправка событий в трекер")
        ok, failed = await send_events(client, args, plans)
        print(f"  Отправлено {ok}, ошибок {failed}")
        if ok == 0:
            raise DemoError("Ни одно событие не принято трекером")

        print(f"\n[5/5] Ожидание данных в ClickHouse (до {args.stats_timeout:g} с)")
        stats = await wait_for_stats(client, args, auth, baseline)

    if stats is None:
        print("\nДанные в /stats так и не появились. Проверьте Redpanda/ClickHouse: "
              "docker compose logs clickhouse track_api")
        return 1
    if baseline != 0:
        print("\nВнимание: отчёт может включать данные прошлых запусков; "
              "для чистого результата укажите новое имя: --experiment <name>")
    print_report(stats, local)
    return 0 if failed == 0 else 1


def main(argv: Optional[list[str]] = None) -> int:
    args = parse_args(argv)
    args.host = args.host.rstrip("/")

    if sys.version_info < (3, 11):
        print("Нужен Python 3.11+ (asyncio.TaskGroup)", file=sys.stderr)
        return 2
    if httpx is None:
        print("Нужен пакет httpx: pip install httpx (или запустите ./venv/bin/python sdk/demo_simulator.py)",
              file=sys.stderr)
        return 2
    token = os.environ.get("SYNAQ_TOKEN", "").strip()
    if not token:
        print("Не задан SYNAQ_TOKEN (JWT сервисного аккаунта с ролью admin).\n"
              "Получите его: eval \"$(make -s token)\"  или  export SYNAQ_TOKEN=\"$(scripts/get_token.sh)\"\n"
              "Либо запустите демо целиком: make demo", file=sys.stderr)
        return 2

    try:
        return asyncio.run(run(args, token))
    except DemoError as exc:
        print(f"\nОшибка: {exc}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("\nПрервано", file=sys.stderr)
        return 130


if __name__ == "__main__":
    sys.exit(main())
