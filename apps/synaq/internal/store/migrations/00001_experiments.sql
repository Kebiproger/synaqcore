-- Таблица experiments. DDL совпадает с тем, что создаёт SQLAlchemy create_all
-- в apps/experiment_api (src/core/database.py), поэтому IF NOT EXISTS: на базе,
-- уже созданной Python-сервисом, миграция ничего не меняет.

-- +goose Up
CREATE TABLE IF NOT EXISTS experiments (
    name VARCHAR(50) NOT NULL,
    variants JSONB NOT NULL,
    config JSONB,
    url VARCHAR(200),
    is_active BOOLEAN NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now() NOT NULL,
    PRIMARY KEY (name)
);

-- +goose Down
-- ВНИМАНИЕ: откат удаляет таблицу вместе со всеми экспериментами (потеря данных).
-- Выполнять только вручную и осознанно; приложение откат не запускает.
DROP TABLE IF EXISTS experiments;
