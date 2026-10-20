-- Только full-режим (Redpanda/Kafka): очередь + MV в tracking_events.
-- Требует 01-tables.sql (выполняется раньше по алфавиту в /docker-entrypoint-initdb.d).
--
-- У колонок Kafka-таблицы не должно быть DEFAULT: движок Kafka их не поддерживает
-- (BAD_ARGUMENTS при CREATE). Отсутствующие в JSON поля и null всё равно становятся
-- значением типа по умолчанию, то есть '' для String.
--
-- kafka_handle_error_mode = 'stream': сообщение, которое не удалось разобрать
-- (не JSON, неверный тип поля), не останавливает чтение топика. Вместо исключения
-- и бесконечного повтора одного и того же батча движок отдаёт строку с текстом ошибки
-- в виртуальной колонке _error (и исходным сообщением в _raw_message), поэтому MV
-- пропускает такие строки (иначе в tracking_events попали бы пустые события с timestamp 1970).
CREATE TABLE IF NOT EXISTS tracking_events_queue (
    timestamp DateTime64(3, 'UTC'),
    user_id String,
    event_id String,
    event_name String,
    experiment_name String,
    variant String,
    event_data String
) ENGINE = Kafka
SETTINGS kafka_broker_list = 'redpanda:9092',
         kafka_topic_list = 'tracking_events',
         kafka_group_name = 'clickhouse_tracking_cg',
         kafka_format = 'JSONEachRow',
         kafka_handle_error_mode = 'stream',
         input_format_null_as_default = 1; -- null превращается в значение по умолчанию ('' для String)

CREATE MATERIALIZED VIEW IF NOT EXISTS tracking_events_mv TO tracking_events AS SELECT
    timestamp,
    user_id,
    event_id,
    event_name,
    experiment_name,
    variant,
    event_data
FROM tracking_events_queue
WHERE length(_error) = 0;
