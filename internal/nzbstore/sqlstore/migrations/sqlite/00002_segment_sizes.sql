-- +goose Up
CREATE TABLE segment (
    message_id TEXT PRIMARY KEY,
    size       INTEGER NOT NULL
);

-- +goose Down
DROP TABLE segment;
