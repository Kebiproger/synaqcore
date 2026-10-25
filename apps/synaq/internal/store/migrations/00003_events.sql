-- Сырые события трекинга в PostgreSQL (EVENT_SINK=postgres, режим по умолчанию).
-- Таблица секционирована по UTC-дню (event_date); секции events.tracking_events_pYYYYMMDD
-- создаёт и удаляет synaq (internal/eventstore), DEFAULT-секции нет: вставка в день
-- без секции падает (SQLSTATE 23514), а не копится в «мусорной» секции.
-- Ключ дедупликации повторяет ORDER BY ClickHouse (event_name, user_id, event_id,
-- toDate(timestamp)); при конфликте остаётся ПЕРВАЯ строка (ON CONFLICT DO NOTHING).
-- Схему events заранее создаёт postgres-app-init (владелец synaq_app); IF NOT EXISTS —
-- для баз без init-скрипта (тесты, запуск от суперпользователя).

-- +goose Up
CREATE SCHEMA IF NOT EXISTS events;

CREATE TABLE events.tracking_events (
    event_date      date        NOT NULL,      -- UTC-день ts; ключ секционирования
    ts              timestamp with time zone NOT NULL,  -- = timestamptz (так sqlc применяет override на time.Time)
    event_id        text        NOT NULL,
    user_id         text        NOT NULL,
    event_name      text        NOT NULL,
    experiment_name text        NOT NULL DEFAULT '',
    variant         text        NOT NULL DEFAULT '',
    event_data      jsonb,
    CONSTRAINT tracking_events_pk PRIMARY KEY (event_id, user_id, event_name, event_date),
    CONSTRAINT tracking_events_day_chk CHECK (event_date = (ts AT TIME ZONE 'UTC')::date)
) PARTITION BY RANGE (event_date);

CREATE INDEX tracking_events_exp_idx ON events.tracking_events (experiment_name, variant, event_name, user_id)
    WHERE experiment_name <> '';

-- +goose Down
-- ВНИМАНИЕ: откат удаляет таблицу вместе со всеми секциями и событиями (потеря данных).
-- Выполнять только вручную и осознанно; приложение откат не запускает.
-- Схема events не удаляется: её создаёт и выдаёт права postgres-app-init.
DROP TABLE IF EXISTS events.tracking_events;
