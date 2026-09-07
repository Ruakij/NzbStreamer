-- +goose Up
-- An archived nzb is one a client removed from its history. It stays presented
-- and stays in this table; only the default history listing leaves it out.
ALTER TABLE nzb ADD COLUMN archived INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE nzb DROP COLUMN archived;
