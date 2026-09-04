-- +goose Up
-- What an nzb presents. The tree is a pure function of the nzb and the settings
-- tree_key hashes, neither of which changes while the process is down, so it is
-- read back on a start instead of walked out of the archive headers again. A row
-- whose key does not match the settings in force is rebuilt and rewrites these.
-- Everything already in the table predates the key and so rebuilds once.
CREATE TABLE nzb_file (
    nzb_name TEXT NOT NULL REFERENCES nzb(name) ON DELETE CASCADE,
    path     TEXT NOT NULL,          -- as presented, the nzb's folder included
    size     INTEGER NOT NULL,       -- the size that would have been reported
    exact    INTEGER NOT NULL,       -- whether that size is measured or a hint
    PRIMARY KEY (nzb_name, path)
);

ALTER TABLE nzb ADD COLUMN tree_key TEXT NOT NULL DEFAULT '';

-- +goose Down
DROP TABLE nzb_file;
ALTER TABLE nzb DROP COLUMN tree_key;
