from pydantic_settings import BaseSettings, SettingsConfigDict

class Settings(BaseSettings):
    app_name: str = "Experiment API"
    REDIS_URL: str

    POSTGRES_HOST: str
    POSTGRES_PORT: int
    POSTGRES_USER: str
    POSTGRES_PASSWORD: str
    POSTGRES_DB: str

    CLICKHOUSE_HOST: str = "clickhouse"
    CLICKHOUSE_HTTP_PORT: int = 8123
    CLICKHOUSE_USER: str = "default"
    CLICKHOUSE_PASSWORD: str = ""

    # Zitadel OIDC: пустые значения → защищённые эндпоинты отвечают 503 (fail closed)
    OIDC_ISSUER: str = ""
    OIDC_JWKS_URL: str = ""
    OIDC_JWKS_HOST: str = ""      # Host-заголовок для JWKS-запроса по внутреннему адресу
    OIDC_CONFIG_FILE: str = ""    # JSON {issuer, project_id, client_id} от bootstrap-контейнера
    OIDC_AUDIENCE: str = ""       # переопределяет project_id из OIDC_CONFIG_FILE

    @property
    def database_url(self) -> str:
        return f"postgresql+asyncpg://{self.POSTGRES_USER}:{self.POSTGRES_PASSWORD}@{self.POSTGRES_HOST}:{self.POSTGRES_PORT}/{self.POSTGRES_DB}"

    model_config = SettingsConfigDict(env_file=".env", env_file_encoding="utf-8", extra="ignore")

settings = Settings()
