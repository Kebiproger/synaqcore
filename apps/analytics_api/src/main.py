"""
Analytics API — Выделенный микросервис для тяжелых аналитических расчетов
"""

from fastapi import FastAPI

from src.api.v1.stats import router as stats_router

app = FastAPI(
    title="SynaqCore Analytics API",
    description="Выделенный сервис агрегаций ClickHouse и статистического анализа A/B-тестов",
    version="1.0.0",
)

# CORS намеренно не настраивается: /api/v1/stats/ вызывается только same-origin
# (дашборд отдаётся тем же gateway), а CORS для публичных маршрутов единолично
# выставляет nginx/nginx.conf.

app.include_router(stats_router, prefix="/api/v1")

@app.get("/health")
async def health_check():
    return {"status": "OK", "service": "analytics_api"}
