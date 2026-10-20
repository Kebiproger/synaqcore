from pydantic_settings import BaseSettings, SettingsConfigDict

class Settings(BaseSettings):
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

    model_config = SettingsConfigDict(env_file=".env", extra="ignore")

settings = Settings()
