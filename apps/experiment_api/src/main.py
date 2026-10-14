from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from src.core.redis import redis_cache
from src.core.database import init_db, dispose_db
from contextlib import asynccontextmanager
from src.api.v1 import admin, client


@asynccontextmanager
async def lifespan(app: FastAPI):
    print("Запускаем Experiment API...")
    await redis_cache.connect()
    await init_db()

    yield

    print("Остонавливаем Experiment API...")
    await redis_cache.close()
    await dispose_db()

app = FastAPI(
    title="A/B Platform - Experiment API",
    description="Микросервис выдачи вариантов для A/B тестов",
    lifespan=lifespan,
)

app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_credentials=False,
    allow_methods=["*"],
    allow_headers=["*"],
)

app.include_router(admin.router, prefix="/api/v1")
app.include_router(client.router, prefix="/api/v1")

@app.get("/")
def read_root():
    return {"message": "Hello, World!"}

@app.get("/health")
def read_health():
    return {"status": "healthy"}