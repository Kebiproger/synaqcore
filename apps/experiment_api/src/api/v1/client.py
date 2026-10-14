import logging
import time

from fastapi import APIRouter, Depends, HTTPException
from sqlalchemy.ext.asyncio import AsyncSession
from src.schemas.experiment import VariantResponse, BatchVariantRequest, BatchVariantResponse
from src.core.redis import redis_cache
from src.core.database import get_session
from src.services.repository import ExperimentRepository
from src.services.allocation import get_user_variant

logger = logging.getLogger(__name__)

router = APIRouter(
    prefix="/variants",
    tags=["Client API (Выдача вариантов)"]
)


# Простейший circuit breaker для Redis: после ошибки не ходим в Redis
# REDIS_BREAKER_COOLDOWN секунд, чтобы при сбое не платить таймаут на каждом запросе
REDIS_BREAKER_COOLDOWN = 5.0
_redis_skip_until: float = 0.0


def _redis_breaker_open() -> bool:
    return time.monotonic() < _redis_skip_until


def _trip_redis_breaker() -> None:
    global _redis_skip_until
    _redis_skip_until = time.monotonic() + REDIS_BREAKER_COOLDOWN


def _reset_redis_breaker() -> None:
    """Сброс состояния breaker (для тестов)"""
    global _redis_skip_until
    _redis_skip_until = 0.0


def _exc_summary(e: Exception) -> str:
    # Только тип и текст ошибки, без конфигурации подключения (REDIS_URL)
    return f"{type(e).__name__}: {e}"


async def _load_config(name: str, session: AsyncSession):
    """Конфиг эксперимента: сначала Redis, при промахе или сбое Redis — PostgreSQL"""
    redis_ok = not _redis_breaker_open()
    exp_config = None
    if redis_ok:
        try:
            exp_config = await redis_cache.get_experiment(name)
        except Exception as e:
            logger.warning(
                "Redis недоступен при чтении %r, fallback на PostgreSQL: %s", name, _exc_summary(e)
            )
            redis_ok = False
            _trip_redis_breaker()

    if exp_config:
        return exp_config

    repo = ExperimentRepository(session)
    exp_config = await repo.get(name)

    if exp_config and redis_ok:
        try:
            await redis_cache.save_experiment(exp_config)
        except Exception as e:
            logger.warning("Не удалось закэшировать %r в Redis: %s", name, _exc_summary(e))
            _trip_redis_breaker()

    return exp_config


@router.get("/config/{experiment_name}")
async def get_experiment_config(
    experiment_name: str,
    session: AsyncSession = Depends(get_session),
):
    """Публичный эндпоинт: конфиг эксперимента для рендеринга на фронтенде"""
    exp_config = await _load_config(experiment_name, session)

    if not exp_config:
        raise HTTPException(status_code=404, detail="Эксперимент не найден")

    return {
        "name": exp_config.name,
        "variants": exp_config.variants,
        "config": exp_config.config or {},
        "url": exp_config.url,
        "is_active": exp_config.is_active,
    }


@router.get("/active")
async def list_active_experiments(session: AsyncSession = Depends(get_session)):
    """Публичный эндпоинт: список активных экспериментов для выбора в клиентских приложениях"""
    repo = ExperimentRepository(session)
    exps = await repo.list_all()
    return [
        {
            "name": e.name,
            "url": e.url,
            "variants": list(e.variants.keys()),
        }
        for e in exps if e.is_active
    ]


@router.get("/eval-configs")
async def get_eval_configs(session: AsyncSession = Depends(get_session)):
    """Публичный эндпоинт: полная карта активных экспериментов с весами вариантов для Local Evaluation в Server SDK"""
    repo = ExperimentRepository(session)
    exps = await repo.list_all()
    return {
        "experiments": {
            e.name: {
                "name": e.name,
                "variants": e.variants,
                "config": e.config or {},
                "url": e.url,
            }
            for e in exps if e.is_active
        }
    }


@router.post("/batch", response_model=BatchVariantResponse)
async def get_variants_batch(
    payload: BatchVariantRequest,
    session: AsyncSession = Depends(get_session),
):
    """Пакетная выдача вариантов для нескольких экспериментов в одном сетевом вызове"""
    result: dict[str, str] = {}
    for exp_name in payload.experiments:
        exp_config = await _load_config(exp_name, session)
        if not exp_config or not exp_config.is_active:
            result[exp_name] = "control"
        else:
            result[exp_name] = get_user_variant(
                user_id=payload.user_id,
                experiment_name=exp_name,
                variants=exp_config.variants,
            )

    return BatchVariantResponse(
        user_id=payload.user_id,
        variants=result,
    )


@router.get("/{experiment_name}", response_model=VariantResponse)
@router.get("/", response_model=VariantResponse)
async def get_variant(
    experiment_name: str | None = None,
    user_id: str = "",
    session: AsyncSession = Depends(get_session),
):
    if not experiment_name or not user_id:
        raise HTTPException(status_code=400, detail="experiment_name и user_id обязательны")

    exp_config = await _load_config(experiment_name, session)

    if not exp_config or not exp_config.is_active:
        return VariantResponse(
            experiment_name=experiment_name,
            user_id=user_id,
            variant="control"
        )

    variant = get_user_variant(
        user_id=user_id,
        experiment_name=experiment_name,
        variants=exp_config.variants
    )

    return VariantResponse(
        experiment_name=experiment_name,
        user_id=user_id,
        variant=variant
    )
