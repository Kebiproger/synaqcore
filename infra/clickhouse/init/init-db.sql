CREATE TABLE IF NOT EXISTS tracking_events (
    timestamp DateTime64(3, 'UTC'),
    user_id String,
    event_id String DEFAULT '',
    event_name String,
    experiment_name String DEFAULT '',
    variant String DEFAULT '',
    event_data String
) ENGINE = ReplacingMergeTree(timestamp)
PARTITION BY toYYYYMM(timestamp)
ORDER BY (event_name, user_id, event_id, toDate(timestamp));

CREATE TABLE IF NOT EXISTS tracking_events_queue (
    timestamp DateTime64(3, 'UTC'),
    user_id String,
    event_id String DEFAULT '',
    event_name String,
    experiment_name String DEFAULT '',
    variant String DEFAULT '',
    event_data String
) ENGINE = Kafka 
SETTINGS kafka_broker_list = 'redpanda:9092',
         kafka_topic_list = 'tracking_events',
         kafka_group_name = 'clickhouse_tracking_cg', 
         kafka_format = 'JSONEachRow',
         input_format_null_as_default=1; -- Превращает null в '' (в нашем случаем)

CREATE MATERIALIZED VIEW IF NOT EXISTS tracking_events_mv TO tracking_events AS SELECT
    timestamp,
    user_id,
    event_id,
    event_name,
    experiment_name,
    variant,
    event_data
FROM tracking_events_queue;