"""
Analytics API — Выделенный микросервис для тяжелых аналитических расчетов
"""

from contextlib import asynccontextmanager

from fastapi import FastAPI

from src.api.v1.stats import router as stats_router
from src.services import events_backend, postgres_aggregator


@asynccontextmanager
async def lifespan(_app: FastAPI):
    # Пул asyncpg нужен только в Postgres-режиме. Если postgres-app ещё не готов,
    # init_pool логирует ошибку, сервис стартует, пул создаётся при первом запросе.
    if events_backend.current_backend() == events_backend.POSTGRES:
        await postgres_aggregator.init_pool()
    try:
        yield
    finally:
        await postgres_aggregator.close_pool()


app = FastAPI(
    title="SynaqCore Analytics API",
    description="Выделенный сервис агрегаций событий (Postgres или ClickHouse) "
                "и статистического анализа A/B-тестов",
    version="1.0.0",
    lifespan=lifespan,
)

# CORS намеренно не настраивается: /api/v1/stats/ вызывается только same-origin
# (дашборд отдаётся тем же gateway), а CORS для публичных маршрутов единолично
# выставляет nginx/nginx.conf.

app.include_router(stats_router, prefix="/api/v1")

@app.get("/health")
async def health_check():
    return {"status": "OK", "service": "analytics_api"}
