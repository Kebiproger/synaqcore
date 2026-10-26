FROM python:3.12-slim

WORKDIR /app

# curl нужен для healthcheck в apps/docker-compose.apps.yml
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl \
    && rm -rf /var/lib/apt/lists/*

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Безопасный non-root пользователь
RUN useradd -m -u 1000 appuser && chown -R appuser:appuser /app
USER appuser

COPY . .

EXPOSE 8000

CMD ["uvicorn", "src.main:app", "--host", "0.0.0.0", "--port", "8000"]
