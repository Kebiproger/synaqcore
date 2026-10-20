import logging

from fastapi import FastAPI
from src.core.redis import redis_cache
from src.core.database import init_db, dispose_db
from contextlib import asynccontextmanager
from src.api.v1 import admin, client

# uvicorn настраивает только свои логгеры; без этого INFO из src.* не выводится.
# Логгеры uvicorn имеют propagate=False, поэтому дублирования строк нет.
logging.basicConfig(
    level=logging.INFO,
    format="%(levelname)s:     %(name)s - %(message)s",
)
logger = logging.getLogger(__name__)


@asynccontextmanager
async def lifespan(app: FastAPI):
    logger.info("Запускаем Experiment API...")
    await redis_cache.connect()
    await init_db()

    yield

    logger.info("Останавливаем Experiment API...")
    await redis_cache.close()
    await dispose_db()

app = FastAPI(
    title="A/B Platform - Experiment API",
    description="Микросервис выдачи вариантов для A/B тестов",
    lifespan=lifespan,
)

# CORS здесь намеренно не настраивается: сервис доступен только через gateway
# (nginx/nginx.conf), который единолично выставляет Access-Control-* для публичных
# маршрутов SDK. Второй слой давал дубль Access-Control-Allow-Origin, и браузеры
# отклоняли кросс-доменные ответы.

app.include_router(admin.router, prefix="/api/v1")
app.include_router(client.router, prefix="/api/v1")

@app.get("/")
def read_root():
    return {"message": "Hello, World!"}

@app.get("/health")
def read_health():
    return {"status": "healthy"}