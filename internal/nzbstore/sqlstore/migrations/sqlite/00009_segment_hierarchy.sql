-- +goose Up
-- What an nzb knows about its own health becomes a hierarchy: the nzb has
-- source files (the files the nzb itself names, the input a check works on), a
-- source file has segments, and a segment can be known missing per server. The
-- old global segment table hung off nothing, so nothing could be asked of it
-- per file and deletion had to walk an explicit list of ids; here deleting the
-- nzb row cascades through the whole of it. Its rows are not carried over: the
-- table was sparse, so what a release loses is sizes and activity counts that
-- rebuild as its segments are touched.
DROP TABLE segment;

CREATE TABLE nzb_source_file (
    id          INTEGER PRIMARY KEY,
    nzb_name    TEXT NOT NULL REFERENCES nzb(name) ON DELETE CASCADE,
    filename    TEXT NOT NULL,     -- as the nzb names it, before any build
    posted_at   INTEGER NOT NULL,  -- what PROBE_MIN_AGE is measured against
    retry_after INTEGER,           -- set when the post was younger than PROBE_MIN_AGE
    UNIQUE (nzb_name, filename)
);

-- Sparse: a row exists only once something is known about that segment, and
-- every writer gets there with INSERT ... ON CONFLICT DO UPDATE, so an add
-- never bulk-writes a row per segment and a row always carries a real answer.
CREATE TABLE segment (
    file_id    INTEGER NOT NULL REFERENCES nzb_source_file(id) ON DELETE CASCADE,
    index_     INTEGER NOT NULL,   -- position in the file, so order is not a join
    message_id TEXT NOT NULL,
    size       INTEGER,            -- null until a fetch measured it
    checked_at INTEGER,            -- when the rotation was asked about it
    present    INTEGER NOT NULL,   -- whether any server had it
    read_at    INTEGER,
    fetched_at INTEGER,
    fetches    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (file_id, index_)
);

-- Only the negative is per server. An article a server dropped does not come
-- back, so the row stays true for as long as the nzb does; a yes expires with
-- retention and is recorded once, on the segment.
CREATE TABLE segment_missing (
    file_id    INTEGER NOT NULL,
    index_     INTEGER NOT NULL,
    server     TEXT NOT NULL,      -- the configured server name
    checked_at INTEGER NOT NULL,
    PRIMARY KEY (file_id, index_, server),
    FOREIGN KEY (file_id, index_) REFERENCES segment(file_id, index_) ON DELETE CASCADE
);

-- Which of the nzb's own files a presented path was built from, so a verdict on
-- a source file can reach the paths it presented.
ALTER TABLE nzb_file ADD COLUMN source_file TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE nzb_file DROP COLUMN source_file;
DROP TABLE segment_missing;
DROP TABLE segment;
DROP TABLE nzb_source_file;
CREATE TABLE segment (
    message_id TEXT PRIMARY KEY,
    size       INTEGER NOT NULL,
    read_at    INTEGER,
    fetched_at INTEGER,
    fetches    INTEGER NOT NULL DEFAULT 1
);
