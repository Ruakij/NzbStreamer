-- +goose Up
-- A row that already exists was fetched once, so the backfill starts the count
-- at one and the next fetch reads as the first re-fetch
ALTER TABLE segment ADD COLUMN read_at    INTEGER;
ALTER TABLE segment ADD COLUMN fetched_at INTEGER;
ALTER TABLE segment ADD COLUMN fetches    INTEGER NOT NULL DEFAULT 1;

-- +goose Down
ALTER TABLE segment DROP COLUMN read_at;
ALTER TABLE segment DROP COLUMN fetched_at;
ALTER TABLE segment DROP COLUMN fetches;
