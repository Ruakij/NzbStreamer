package main

import (
	"log/slog"
	"regexp"
	"time"
)

// UsenetServerConfig is one news server. It is read under a prefix: USENET_ for
// the first server and USENET_<n>_ for every further one, so the variable names
// of a single-server setup are unchanged.
type UsenetServerConfig struct {
	Host        string        `env:"HOST, required"`             // Usenet server host
	Port        int           `env:"PORT, default=563"`          // Usenet server port
	TLS         bool          `env:"TLS, default=true"`          // Use TLS for Usenet connection
	User        string        `env:"USER, required"`             // Usenet username
	Password    string        `env:"PASS, required"`             // Usenet password
	MaxConn     int           `env:"MAX_CONN, default=20"`       // Maximum Usenet connections to use
	Priority    int           `env:"PRIORITY"`                   // Priority, lower is chosen first; servers sharing a priority share the load round robin. Defaults to 1 for the unindexed server and to its index for the others
	QuotaBytes  Bytes         `env:"QUOTA_BYTES"`                // Bytes this server may serve per period; 0 is unmetered. A server whose quota is spent is skipped as if it were not configured
	QuotaPeriod time.Duration `env:"QUOTA_PERIOD, default=720h"` // How long a quota lasts before it resets; the reset happens on the first fetch after it ends
	Probe       bool          `env:"PROBE, default=true"`        // Connect to the server at startup, so rejected credentials and a host that is unreachable are reported then rather than by the first read that needs it
}

// UsenetConfig is what every server shares: they are properties of how this
// program behaves, not of an account. The servers themselves are read by
// usenetServers.
type UsenetConfig struct {
	MaxAttempts  int           `env:"USENET_MAX_ATTEMPTS, default=3"`   // Attempts a request gets before its error is reported
	RetryBackoff time.Duration `env:"USENET_RETRY_BACKOFF, default=1s"` // Wait after the first failed attempt, doubled after each further one
	Timeout      time.Duration `env:"USENET_TIMEOUT, default=30s"`      // Timeout for connecting and for completing a single request
	IdleTimeout  time.Duration `env:"USENET_IDLE_TIMEOUT, default=2m"`  // Time after which an unused connection is closed; 0 or less falls back to the default

	BreakerFailures int           `env:"USENET_BREAKER_FAILURES, default=3"`  // Consecutive failed requests that disable a server, so the others carry the load instead of every request descending past it; 0 never disables one for failures. Rejected credentials disable it for the rest of the process whatever this is set to, and the accounts connection limit is only logged, since using fewer connections is the only fix for it
	BreakerCooldown time.Duration `env:"USENET_BREAKER_COOLDOWN, default=5m"` // How long a disabled server is skipped for; the first request after it decides whether it is disabled again
}

type HTTPConfig struct {
	Address string `env:"HTTP_ADDRESS, default=:8080"` // Address the process listens on; serves the web ui, its api, /sabnzbd/api and /webdav/
	Debug   bool   `env:"HTTP_DEBUG, default=false"`   // Serve /debug/pprof/ and /debug/statsviz/
}

type WebdavConfig struct {
	Username      string `env:"WEBDAV_USERNAME"`                      // Username for WebDAV basic auth; Authentication disabled when unset
	Password      string `env:"WEBDAV_PASSWORD"`                      // Password for WebDAV basic auth
	LazyExactSize bool   `env:"WEBDAV_LAZY_EXACT_SIZE, default=true"` // Measure the exact size of a file on the GET that needs it, where doing so is cheap, so Content-Length is right for one NZB_EAGER_EXACT_SIZE_CLASSES left out; disabling it answers every GET from the size hint, which for an estimated size means a truncated response
}

type SabnzbdConfig struct {
	APIKey      string   `env:"SABNZBD_API_KEY"`                         // Api key demanded of every request; unauthenticated when unset
	CompleteDir string   `env:"SABNZBD_COMPLETE_DIR"`                    // Path reported to a client as the completed-downloads folder, which is where it imports from; defaults to MOUNT_PATH
	Categories  []string `env:"SABNZBD_CATEGORIES, default=*,tv,movies"` // Categories offered to a client; it refuses to save if the one it is configured with is missing
}

type MountConfig struct {
	Path    string   `env:"MOUNT_PATH"`    // Path for FUSE mount; Disabled when unset
	Options []string `env:"MOUNT_OPTIONS"` // Additional Options for FUSE mount; See mount.fuse3 Manpage for more information
	// A read here waits on a news server, so these decide the throughput of a
	// sequential read rather than the connections do
	MaxBackground int   `env:"MOUNT_MAX_BACKGROUND, default=64"` // Reads the kernel may have in flight per mount
	MaxReadAhead  Bytes `env:"MOUNT_MAX_READAHEAD, default=8M"`  // Bytes the kernel reads ahead of a sequential reader; a request is capped at 1 MiB, so this is how many it issues
}

type CacheConfig struct {
	Path    string `env:"CACHE_PATH, default=.cache"` // Path for segment-cache
	MaxSize Bytes  `env:"CACHE_MAX_SIZE, default=0"`  // Maximum cache size, if unset allows unlimited size (not recommended)
}

type MetadataConfig struct {
	Path string `env:"METADATA_PATH, default=.metadata/metadata.db"` // Path for the metadata database; WAL puts two sibling files next to it
}

type ReadaheadConfig struct {
	Size  Bytes `env:"READAHEAD_SIZE, default=32M"` // Bytes held warm ahead of each open file; 0 disables readahead
	Chunk Bytes `env:"READAHEAD_CHUNK, default=1M"` // Bytes fetched per chunk; SIZE/CHUNK is how many run at once, and one chunk is served segment by segment, so around one segment reads fastest
}

type FolderWatcherConfig struct {
	Path    string `env:"FOLDER_WATCHER_PATH, default=.watch"`  // Watch folder for adding nzbs
	Consume bool   `env:"FOLDER_WATCHER_CONSUME, default=true"` // Delete an nzb file once it has been added; the metadata database keeps it
}

type NzbConfig struct {
	FileBlacklist         []regexp.Regexp `env:"NZB_FILE_BLACKLIST, default="`                  // Early Regex-blacklist, applied after the nzb-file is scanned; a file dropped here is not health-checked either
	ProbeSizeConvention   int             `env:"NZB_PROBE_SIZE_CONVENTION, default=3"`          // Segments of an nzb whose size hints do not identify what they count that may be downloaded to settle it, making its sizes exact; one that settles nothing costs the next attempt, 0 leaves the sizes as estimates until a read has measured them
	EagerExactSizeClasses []string        `env:"NZB_EAGER_EXACT_SIZE_CLASSES, default=content"` // File classes measured as part of an add, so a listing reports their exact size before anything has read them: content, recovery, other, or empty for none. A file posted as it is costs one segment; a member of an archive knows its length from the header and costs nothing. Whatever is left out is measured on its first read instead
	MaxArchiveDepth       int             `env:"NZB_MAX_ARCHIVE_DEPTH, default=2"`              // Archives unpacked on top of each other, for an upload that packed a rar set inside a rar set; one nested deeper is presented as the volumes it is and the add still finishes. Each level costs a walk of the archives block headers, so this bounds what an add spends on metadata
	Concurrency           int             `env:"NZB_CONCURRENCY, default=4"`                    // Nzbs built at once, whether added or restored on startup; the rest wait in the queue. Building one is mostly waiting on the news server, over connections every read shares; 0 or less is unbounded
}

type ProbeConfig struct {
	InitialFilePercent       float64 `env:"PROBE_INITIAL_FILE_PERCENT, default=0.5"`        // Segments checked per content file on the first pass, as a percentage of its segments, spread evenly; 0 disables checking
	InitialFileMinSegments   int     `env:"PROBE_INITIAL_FILE_MIN_SEGMENTS, default=2"`     // Floor on that sample, so a short file is not rounded down to nothing
	InitialFileMaxSegments   int     `env:"PROBE_INITIAL_FILE_MAX_SEGMENTS, default=8"`     // Cap on that sample, so a huge file does not turn the add into a download
	ExtensiveFilePercent     float64 `env:"PROBE_EXTENSIVE_FILE_PERCENT, default=1.0"`      // Ceiling on the widened sample a file gets when the first pass cannot decide it; 0 skips the second pass
	ExtensiveFileMaxSegments int     `env:"PROBE_EXTENSIVE_FILE_MAX_SEGMENTS, default=512"` // Absolute cap on that widened sample
	MaxMissingPercent        float64 `env:"PROBE_MAX_MISSING_PERCENT, default=100"`         // Ceiling on accepted damage regardless of par2; 100 lets par2 capacity govern on its own
	Par2Safety               float64 `env:"PROBE_PAR2_SAFETY, default=0.9"`                 // Fraction of the estimated par2 capacity to trust, since the capacity is itself estimated
	UndecidedAccept          bool    `env:"PROBE_UNDECIDED_ACCEPT, default=true"`           // Accept a file the second pass still cannot decide
	Confidence               float64 `env:"PROBE_CONFIDENCE, default=0.95"`                 // Confidence of the interval the verdict is taken from; lower means fewer escalations and more wrong calls
	Parallel                 int     `env:"PROBE_PARALLEL, default=0"`                      // Concurrent segment-checks; defaults to the sum of the servers connections when 0
}

type FilesystemConfig struct {
	Blacklist            []regexp.Regexp `env:"FILESYSTEM_BLACKLIST, default=(?i)\\.par2$"`     // Late Regex-blacklist, applied on the actual file added to the filesystem; includes files from archives
	FlattenMaxDepth      int             `env:"FILESYSTEM_FLATTEN_MAX_DEPTH, default=0"`        // Unpacks files from folders e.g. archives where possible
	FixFilenameThreshold float32         `env:"FILESYSTEM_FIX_FILENAME_THRESHOLD, default=0.2"` // Threshold for applying filename-fixing when filename doesnt match nzb meta name
}

type LoggingConfig struct {
	Level slog.Level `env:"LOGLEVEL, default=INFO"` // Logging level, one of {DEBUG, INFO, WARN, ERROR}
}

type Config struct {
	Usenet        UsenetConfig
	Mount         MountConfig
	HTTP          HTTPConfig
	Webdav        WebdavConfig
	Sabnzbd       SabnzbdConfig
	Cache         CacheConfig
	Metadata      MetadataConfig
	Readahead     ReadaheadConfig
	NzbConfig     NzbConfig
	Probe         ProbeConfig
	Filesystem    FilesystemConfig
	FolderWatcher FolderWatcherConfig
	Logging       LoggingConfig
}
