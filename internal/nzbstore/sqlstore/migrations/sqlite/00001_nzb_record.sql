-- +goose Up
CREATE TABLE nzb (
    name     TEXT PRIMARY KEY,
    raw      BLOB NOT NULL,
    added_at INTEGER NOT NULL
);

-- +goose Down
DROP TABLE nzb;
