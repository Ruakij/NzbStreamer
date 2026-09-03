package main

import (
	"log/slog"
	"regexp"
	"time"
)

type UsenetConfig struct {
	Host     string `env:"USENET_HOST, required"`       // Usenet server host
	Port     int    `env:"USENET_PORT, default=563"`    // Usenet server port
	TLS      bool   `env:"USENET_TLS, default=true"`    // Use TLS for Usenet connection
	User     string `env:"USENET_USER, required"`       // Usenet username
	Password string `env:"USENET_PASS, required"`       // Usenet password
	MaxConn  int    `env:"USENET_MAX_CONN, default=20"` // Maximum Usenet connections to use

	MaxAttempts  int           `env:"USENET_MAX_ATTEMPTS, default=3"`   // Attempts a request gets before its error is reported
	RetryBackoff time.Duration `env:"USENET_RETRY_BACKOFF, default=1s"` // Wait after the first failed attempt, doubled after each further one
	Timeout      time.Duration `env:"USENET_TIMEOUT, default=30s"`      // Timeout for connecting and for completing a single request
	IdleTimeout  time.Duration `env:"USENET_IDLE_TIMEOUT, default=2m"`  // Time after which an unused connection is closed; 0 or less falls back to the default
}

type WebdavConfig struct {
	Address  string `env:"WEBDAV_ADDRESS, default=:8080"` // Address for WebDAV server; Disabled when unset
	Username string `env:"WEBDAV_USERNAME"`               // Username for WebDAV basic auth; Authentication disabled when unset
	Password string `env:"WEBDAV_PASSWORD"`               // Password for WebDAV basic auth
}

type MountConfig struct {
	Path    string   `env:"MOUNT_PATH"`    // Path for FUSE mount; Disabled when unset
	Options []string `env:"MOUNT_OPTIONS"` // Additional Options for FUSE mount; See mount.fuse3 Manpage for more information
}

type CacheConfig struct {
	Path    string `env:"CACHE_PATH, default=.cache"` // Path for segment-cache
	MaxSize int64  `env:"CACHE_MAX_SIZE, default=0"`  // Maximum cache size in bytes, if unset allows unlimited size (not recommended)
}

type MetadataConfig struct {
	Path string `env:"METADATA_PATH, default=.metadata/metadata.db"` // Path for the metadata database; WAL puts two sibling files next to it
}

type PrefetchConfig struct {
	Time        time.Duration `env:"PREFETCH_TIME, default=1s"`         // How far ahead of the read position to stay warm, in read-time
	MinSegments int           `env:"PREFETCH_MIN_SEGMENTS, default=8"`  // Segments warmed ahead before a read speed can be measured
	MaxSegments int           `env:"PREFETCH_MAX_SEGMENTS, default=64"` // Upper bound on segments warmed ahead; 0 disables prefetching
	MaxConn     int           `env:"PREFETCH_MAX_CONN, default=0"`      // Concurrent prefetches across all files; defaults to USENET_MAX_CONN when 0
}

type FolderWatcherConfig struct {
	Path    string `env:"FOLDER_WATCHER_PATH, default=.watch"`   // Watch folder for adding nzbs
	Consume bool   `env:"FOLDER_WATCHER_CONSUME, default=true"`  // Delete an nzb file once it has been added; the metadata database keeps it
}

type NzbConfig struct {
	FileBlacklist         []regexp.Regexp `env:"NZB_FILE_BLACKLIST, default=(?i)\\.par2$"` // Early Regex-blacklist, immediately applied after nzb-file is scanned
	SegmentsPerFile       int             `env:"NZB_CHECK_SEGMENTS_PER_FILE, default=2"`   // Segments checked per file for existence, spread evenly; 0 disables the check, -1 checks all
	SegmentCheckParallel  int             `env:"NZB_CHECK_SEGMENTS_PARALLEL, default=0"`   // Concurrent segment-checks; defaults to USENET_MAX_CONN when 0
	FilesHealthyThreshold float32         `env:"NZB_FILES_HEALTHY_THRESHOLD, default=1.0"` // Above this percentage-threshold, files with missing segments are allowed
}

type FilesystemConfig struct {
	Blacklist            []regexp.Regexp `env:"FILESYSTEM_BLACKLIST, default="`                 // Late Regex-blacklist, applied on the actual file added to the filesystem; includes files from archives
	FlattenMaxDepth      int             `env:"FILESYSTEM_FLATTEN_MAX_DEPTH, default=1"`        // Unpacks files from folders e.g. archives where possible
	FixFilenameThreshold float32         `env:"FILESYSTEM_FIX_FILENAME_THRESHOLD, default=0.2"` // Threshold for applying filename-fixing when filename doesnt match nzb meta name
}

type LoggingConfig struct {
	Level slog.Level `env:"LOGLEVEL, default=INFO"` // Logging level, one of {DEBUG, INFO, WARN, ERROR}
}

type Config struct {
	Usenet        UsenetConfig
	Mount         MountConfig
	Webdav        WebdavConfig
	Cache         CacheConfig
	Metadata      MetadataConfig
	Prefetch      PrefetchConfig
	NzbConfig     NzbConfig
	Filesystem    FilesystemConfig
	FolderWatcher FolderWatcherConfig
	Logging       LoggingConfig
}
