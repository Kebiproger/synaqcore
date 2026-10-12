from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import HTMLResponse
from src.core.config import settings
from src.core.redis import redis_cache
from src.core.database import init_db, dispose_db
from contextlib import asynccontextmanager
from src.api.v1 import admin, client, stats
from pathlib import Path


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
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)

app.include_router(admin.router, prefix="/api/v1")
app.include_router(client.router, prefix="/api/v1")
app.include_router(stats.router, prefix="/api/v1")

@app.get("/admin", response_class=HTMLResponse)
@app.get("/admin/", response_class=HTMLResponse)
def admin_page():
    return Path("src/templates/admin.html").read_text(encoding="utf-8")

@app.get("/")
def read_root():
    return {"message": "Hello, World!"}

@app.get("/health")
def read_health():
    return {"status": "healthy"}