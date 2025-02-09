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

type ReadaheadCacheConfig struct {
	AvgSpeedTime time.Duration `env:"READAHEAD_CACHE_AVG_SPEED_TIME, default=0.5s"` // Time over which average read speed is calculated
	Time         time.Duration `env:"READAHEAD_CACHE_TIME, default=1s"`             // Readahead time
	MinSize      int           `env:"READAHEAD_CACHE_MIN_SIZE, default=1048576"`    // Minimum readahead amount in bytes
	LowBuffer    int           `env:"READAHEAD_CACHE_LOW_BUFFER, default=1048576"`  // Buffer size that triggers readahead in bytes
	MaxSize      int           `env:"READAHEAD_CACHE_MAX_SIZE, default=16777216"`   // Maximum readahead amount in bytes; Disables readahead-cache when 0
}

type FolderWatcherConfig struct {
	Path string `env:"FOLDER_WATCHER_PATH, default=.watch"` // Watch folder for adding nzbs (blackhole folder)
}

type NzbConfig struct {
	FileBlacklist         []regexp.Regexp `env:"NZB_FILE_BLACKLIST, default=(?i)\\.par2$"` // Early Regex-blacklist, immediately applied after nzb-file is scanned
	TryReadBytes          int64           `env:"NZB_TRY_READ_BYTES, default=1"`            // Bytes to try to read when scanning files
	TryReadPercentage     float32         `env:"NZB_TRY_READ_PERCENTAGE, default=0"`       // Percentage of file to try to read when scanning files
	FilesHealthyThreshold float32         `env:"NZB_FILES_HEALTHY_THRESHOLD, default=1.0"` // Above this percentage-threshold, try-read errors are allowed
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
	Usenet         UsenetConfig
	Mount          MountConfig
	Webdav         WebdavConfig
	Cache          CacheConfig
	ReadaheadCache ReadaheadCacheConfig
	NzbConfig      NzbConfig
	Filesystem     FilesystemConfig
	FolderWatcher  FolderWatcherConfig
	Logging        LoggingConfig
}
