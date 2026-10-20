-- Хранилище событий. synaq (Go) пишет сюда напрямую батчами по HTTP (INSERT),
-- без Kafka. Дедупликация — ReplacingMergeTree по event_id в ORDER BY.
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
