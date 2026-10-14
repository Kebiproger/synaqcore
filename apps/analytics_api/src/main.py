"""
Analytics API — Выделенный микросервис для тяжелых аналитических расчетов
"""

from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware

from src.core.config import settings
from src.api.v1.stats import router as stats_router

app = FastAPI(
    title="SynaqCore Analytics API",
    description="Выделенный сервис агрегаций ClickHouse и статистического анализа A/B-тестов",
    version="1.0.0",
)

# CORS
origins = [o.strip() for o in settings.CORS_ORIGINS.split(",") if o.strip()]
if not origins or origins == ["*"]:
    app.add_middleware(
        CORSMiddleware,
        allow_origins=["*"],
        allow_credentials=False,
        allow_methods=["*"],
        allow_headers=["*"],
    )
else:
    app.add_middleware(
        CORSMiddleware,
        allow_origins=origins,
        allow_credentials=True,
        allow_methods=["*"],
        allow_headers=["*"],
    )

app.include_router(stats_router, prefix="/api/v1")

@app.get("/health")
async def health_check():
    return {"status": "OK", "service": "analytics_api"}
