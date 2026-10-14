from typing import Optional
from sqlalchemy import select, exists
from sqlalchemy.ext.asyncio import AsyncSession
from src.core.database import ExperimentModel
from src.schemas.experiment import ExperimentConfigCreate


class ActiveVariantsChangeError(Exception):
    """Попытка изменить варианты активного эксперимента (перетасует пользователей)."""


def _to_schema(model: ExperimentModel) -> ExperimentConfigCreate:
    # Данные из БД доверенные; model_construct не валидирует, чтобы строки, созданные
    # до ужесточения схемы (например, старые имена), не роняли чтение.
    return ExperimentConfigCreate.model_construct(
        name=model.name,
        variants=model.variants,
        config=model.config,
        url=model.url,
        is_active=model.is_active,
    )


class ExperimentRepository:
    def __init__(self, session: AsyncSession):
        self.session = session

    async def get(self, name: str) -> Optional[ExperimentConfigCreate]:
        result = await self.session.get(ExperimentModel, name)
        if not result:
            return None
        return _to_schema(result)

    async def create(self, experiment: ExperimentConfigCreate) -> None:
        model = ExperimentModel(
            name=experiment.name,
            variants=experiment.variants,
            config=experiment.config,
            url=experiment.url,
            is_active=experiment.is_active,
        )
        self.session.add(model)
        await self.session.commit()

    async def exists(self, name: str) -> bool:
        query = select(exists().where(ExperimentModel.name == name))
        result = await self.session.execute(query)
        return result.scalar()

    async def list_all(self) -> list[ExperimentConfigCreate]:
        result = await self.session.execute(select(ExperimentModel).order_by(ExperimentModel.created_at.desc()))
        rows = result.scalars().all()
        return [_to_schema(r) for r in rows]

    _UPDATABLE_FIELDS = frozenset({"variants", "config", "url", "is_active"})

    async def update(self, name: str, **fields) -> Optional[ExperimentConfigCreate]:
        """Частично обновить эксперимент. None — если эксперимента нет.

        ActiveVariantsChangeError — если меняются варианты, а эксперимент активен
        (или остаётся активным после обновления): аллокация перетасует пользователей.
        """
        unknown = set(fields) - self._UPDATABLE_FIELDS
        if unknown:
            raise ValueError(f"Нельзя обновить поля: {sorted(unknown)}")
        # FOR UPDATE: проверка активности и запись атомарны относительно параллельных PATCH
        model = await self.session.get(ExperimentModel, name, with_for_update=True)
        if not model:
            return None
        if "variants" in fields and fields["variants"] != model.variants:
            stays_active = fields.get("is_active", model.is_active)
            if stays_active:
                await self.session.rollback()
                raise ActiveVariantsChangeError(name)
        for key, value in fields.items():
            setattr(model, key, value)
        await self.session.commit()
        return _to_schema(model)

    async def delete(self, name: str) -> bool:
        model = await self.session.get(ExperimentModel, name)
        if not model:
            return False
        await self.session.delete(model)
        await self.session.commit()
        return True
