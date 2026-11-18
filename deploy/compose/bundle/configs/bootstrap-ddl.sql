-- Bootstrap schema for local Docker Compose testing.
CREATE TABLE IF NOT EXISTS default.events
(
    event_time DateTime DEFAULT now(),
    source LowCardinality(String),
    message String
)
ENGINE = ReplicatedMergeTree('/clickhouse/tables/{shard}/default/events', '{replica}')
ORDER BY (source, event_time);
