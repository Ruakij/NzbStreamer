-- +goose Up
-- The category a download client api was given when the nzb was added. It means
-- nothing here and is only reported back, but a client filters its own downloads
-- by it, so an nzb that loses its category over a restart is one that client
-- stops seeing.
ALTER TABLE nzb ADD COLUMN category TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE nzb DROP COLUMN category;
