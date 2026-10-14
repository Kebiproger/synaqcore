from pydantic import BaseModel, ConfigDict, Field, StringConstraints, field_validator, model_validator
from typing import Annotated, Dict, Optional, Any

# Имя эксперимента попадает в URL, ключи Redis, SQL ClickHouse и HTML дашборда —
# поэтому только безопасный набор символов (защита от XSS/инъекций ниже по цепочке).
EXPERIMENT_NAME_PATTERN = r"^[A-Za-z0-9_.-]+$"
# Совпадают с системными путями client роутера
RESERVED_EXPERIMENT_NAMES = frozenset({"active", "config", "batch", "eval-configs"})

VARIANT_KEY_PATTERN = r"^[A-Za-z0-9_-]+$"
MAX_VARIANTS = 20

ExperimentName = Annotated[str, StringConstraints(min_length=1, max_length=50, pattern=EXPERIMENT_NAME_PATTERN)]
VariantKey = Annotated[str, StringConstraints(min_length=1, max_length=50, pattern=VARIANT_KEY_PATTERN)]


def _check_weights_positive(v: Optional[Dict[str, int]]) -> Optional[Dict[str, int]]:
    if v is not None and any(w <= 0 for w in v.values()):
        raise ValueError("Все веса должны быть положительными")
    return v


class ExperimentConfigCreate(BaseModel):
    name: ExperimentName = Field(..., description="Имя эксперимента: латиница, цифры, '_', '.', '-'")
    variants: Dict[VariantKey, int] = Field(..., min_length=1, max_length=MAX_VARIANTS,
                                            description="Веса вариантов. Пример: {'A': 50, 'B': 50}",
                                            json_schema_extra={"example": {"A": 33, "B": 33, "C": 33}}
                                            )
    config: Optional[Dict[str, Any]] = Field(default=None, description="Конфиг для рендеринга вариантов. Ключи — имена вариантов")
    url: Optional[str] = Field(default=None, max_length=200, description="URL-паттерн для привязки эксперимента к странице")
    is_active: bool = Field(default=True, description="Включен ли эксперимент")

    @field_validator('name')
    @classmethod
    def check_name_not_reserved(cls, v):
        if v.lower() in RESERVED_EXPERIMENT_NAMES:
            raise ValueError(f"Имя '{v}' зарезервировано")
        return v

    @field_validator('variants')
    @classmethod
    def check_weights_positive(cls, v):
        return _check_weights_positive(v)


class ExperimentUpdate(BaseModel):
    """Частичное обновление эксперимента (PATCH). Имя менять нельзя."""
    model_config = ConfigDict(extra="forbid")

    is_active: Optional[bool] = Field(default=None, description="Включен ли эксперимент")
    variants: Optional[Dict[VariantKey, int]] = Field(default=None, min_length=1, max_length=MAX_VARIANTS,
                                                      description="Новые веса вариантов")
    config: Optional[Dict[str, Any]] = Field(default=None, description="Конфиг рендеринга (null — очистить)")
    url: Optional[str] = Field(default=None, max_length=200, description="URL-паттерн (null — очистить)")

    @field_validator('variants')
    @classmethod
    def check_weights_positive(cls, v):
        return _check_weights_positive(v)

    @model_validator(mode="after")
    def check_not_null(self):
        # is_active и variants в БД NOT NULL: явный null недопустим
        for name in ("is_active", "variants"):
            if name in self.model_fields_set and getattr(self, name) is None:
                raise ValueError(f"Поле '{name}' не может быть null")
        return self


class VariantResponse(BaseModel):
    experiment_name: str = Field(..., description="имя запрошенного теста")
    user_id: str = Field(..., min_length=1, description="ID пользователя")
    variant: str = Field(..., description="Выпавший вариант (A, B, control и т.д.)")


class BatchVariantRequest(BaseModel):
    user_id: str = Field(..., min_length=1, description="ID пользователя")
    experiments: list[ExperimentName] = Field(..., min_length=1, max_length=50, description="Список имен экспериментов")


class BatchVariantResponse(BaseModel):
    user_id: str = Field(..., description="ID пользователя")
    variants: Dict[str, str] = Field(..., description="Словарь {experiment_name: variant}")

