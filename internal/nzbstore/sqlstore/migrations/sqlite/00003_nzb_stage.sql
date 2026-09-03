-- +goose Up
-- An nzb is recorded when it is accepted rather than when it is built, so an add
-- interrupted by a restart is resumed and one that failed is still reportable.
-- The stage rides on the nzb row because the record of an add and the nzb it
-- added are the same thing: dropping one without the other would leave either an
-- nzb nothing can report on or a history entry for files nobody can reach.
-- Everything already in the table was written by an add that succeeded.
ALTER TABLE nzb ADD COLUMN stage TEXT NOT NULL DEFAULT 'completed';
ALTER TABLE nzb ADD COLUMN error TEXT NOT NULL DEFAULT '';
ALTER TABLE nzb ADD COLUMN finished_at INTEGER;

-- +goose Down
ALTER TABLE nzb DROP COLUMN stage;
ALTER TABLE nzb DROP COLUMN error;
ALTER TABLE nzb DROP COLUMN finished_at;
