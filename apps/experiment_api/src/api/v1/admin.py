import logging
from fastapi import APIRouter, Depends, HTTPException
from sqlalchemy.ext.asyncio import AsyncSession
from src.schemas.experiment import ExperimentConfigCreate, ExperimentUpdate
from src.core.auth import require_roles
from src.core.database import get_session
from src.core.redis import redis_cache
from src.services.repository import ActiveVariantsChangeError, ExperimentRepository

logger = logging.getLogger(__name__)

router = APIRouter(prefix='/experiments', tags=['Admin API'])

# Роли из Zitadel OIDC (см. src/core/auth.py)
require_admin = require_roles("admin")
require_viewer = require_roles("admin", "analyst")


def _experiment_to_dict(e: ExperimentConfigCreate) -> dict:
    return {
        "name": e.name,
        "variants": e.variants,
        "config": e.config,
        "url": e.url,
        "is_active": e.is_active,
        "total_weight": sum(e.variants.values()),
    }


async def _cache_experiment(experiment: ExperimentConfigCreate) -> None:
    """Записать конфиг в Redis; при ошибке — best-effort удалить ключ,
    чтобы устаревший конфиг не отдавался до истечения TTL."""
    try:
        await redis_cache.save_experiment(experiment)
        return
    except Exception as e:
        logger.warning("Не удалось закэшировать '%s' в Redis: %s", experiment.name, e)
    try:
        await redis_cache.delete_experiment(experiment.name)
    except Exception as e:
        logger.warning("Не удалось удалить устаревший '%s' из кэша Redis: %s", experiment.name, e)


@router.post("/", status_code=201, dependencies=[Depends(require_admin)])
async def create_experiment(
    experiment: ExperimentConfigCreate,
    session: AsyncSession = Depends(get_session),
):
    repo = ExperimentRepository(session)
    if await repo.exists(experiment.name):
        raise HTTPException(
            status_code=400,
            detail=f"Эксперимент '{experiment.name}' уже существует!"
        )
    await repo.create(experiment)
    await _cache_experiment(experiment)
    return {"message": "Эксперимент успешно создан", "name": experiment.name}


@router.get("/", dependencies=[Depends(require_viewer)])
async def list_experiments(session: AsyncSession = Depends(get_session)):
    repo = ExperimentRepository(session)
    exps = await repo.list_all()
    return [_experiment_to_dict(e) for e in exps]


@router.patch("/{name}", dependencies=[Depends(require_admin)])
async def update_experiment(
    name: str,
    update: ExperimentUpdate,
    session: AsyncSession = Depends(get_session),
):
    fields = update.model_dump(exclude_unset=True)
    if not fields:
        raise HTTPException(status_code=400, detail="Нет полей для обновления")
    repo = ExperimentRepository(session)
    try:
        updated = await repo.update(name, **fields)
    except ActiveVariantsChangeError:
        raise HTTPException(
            status_code=409,
            detail="Нельзя менять варианты активного эксперимента: пользователи будут перераспределены. "
                   "Сначала выключите эксперимент (можно в том же запросе: is_active=false).",
        )
    if updated is None:
        raise HTTPException(status_code=404, detail="Эксперимент не найден")
    await _cache_experiment(updated)
    return _experiment_to_dict(updated)


@router.delete("/{name}", dependencies=[Depends(require_admin)])
async def delete_experiment(
    name: str,
    session: AsyncSession = Depends(get_session),
):
    repo = ExperimentRepository(session)
    if not await repo.delete(name):
        raise HTTPException(status_code=404, detail="Эксперимент не найден")
    try:
        await redis_cache.delete_experiment(name)
    except Exception as e:
        logger.warning("Не удалось удалить '%s' из кэша Redis: %s", name, e)
    return {"message": f"Эксперимент '{name}' удалён"}
