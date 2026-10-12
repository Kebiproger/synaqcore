from fastapi import APIRouter, Depends, HTTPException
from sqlalchemy.ext.asyncio import AsyncSession
from src.schemas.experiment import VariantResponse
from src.core.redis import redis_cache
from src.core.database import get_session
from src.services.repository import ExperimentRepository
from src.services.allocation import get_user_variant

router = APIRouter(
    prefix="/variants",
    tags=["Client API (Выдача вариантов)"]
)


@router.get("/config/{experiment_name}")
async def get_experiment_config(
    experiment_name: str,
    session: AsyncSession = Depends(get_session),
):
    """Публичный эндпоинт: конфиг эксперимента для рендеринга на фронтенде"""
    exp_config = await redis_cache.get_experiment(experiment_name)
    if not exp_config:
        repo = ExperimentRepository(session)
        exp_config = await repo.get(experiment_name)
        if exp_config:
            await redis_cache.save_experiment(exp_config)

    if not exp_config:
        raise HTTPException(status_code=404, detail="Эксперимент не найден")

    return {
        "name": exp_config.name,
        "variants": exp_config.variants,
        "config": exp_config.config or {},
        "url": exp_config.url,
        "is_active": exp_config.is_active,
    }

@router.get("/", response_model=VariantResponse)
async def get_variant(
    experiment_name: str,
    user_id: str,
    session: AsyncSession = Depends(get_session),
):
    exp_config = await redis_cache.get_experiment(experiment_name)

    if not exp_config:
        repo = ExperimentRepository(session)
        exp_config = await repo.get(experiment_name)
        if exp_config:
            await redis_cache.save_experiment(exp_config)

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