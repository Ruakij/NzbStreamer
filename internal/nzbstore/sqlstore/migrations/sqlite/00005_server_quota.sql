-- +goose Up
CREATE TABLE server (
    name         TEXT PRIMARY KEY,
    used_bytes   INTEGER NOT NULL,
    period_start INTEGER NOT NULL
);

-- +goose Down
DROP TABLE server;
