package main

import (
	"regexp"
	"time"
)

type UsenetConfig struct {
	Host     string `env:"USENET_HOST, required"`
	Port     int    `env:"USENET_PORT, default=563"`
	Tls      bool   `env:"USENET_TLS, default=true"`
	User     string `env:"USENET_USER, required"`
	Password string `env:"USENET_USER, required"`
	MaxConn  int    `env:"USENET_MAX_CONN, default=20"`
}

type WebdavConfig struct {
	Address string `env:"WEBDAV_ADDRESS, default=:8080"`
}

type CacheConfig struct {
	Path    string `env:"CACHE_PATH, default=.cache"`
	MaxSize int64  `env:"CACHE_MAX_SIZE, default=-1"`
}

type ReadaheadCacheConfig struct {
	AvgSpeedTime time.Duration `env:"READAHEAD_CACHE_AVG_SPEED_TIME, default=0.5s"`
	Time         time.Duration `env:"READAHEAD_CACHE_TIME, default=1s"`
	MinSize      int           `env:"READAHEAD_CACHE_MIN_SIZE, default=1048576"`
	LowBuffer    int           `env:"READAHEAD_CACHE_LOW_BUFFER, default=1048576"`
	MaxSize      int           `env:"READAHEAD_CACHE_MAX_SIZE, default=16777216"`
}

type FolderWatcherConfig struct {
	Path string `env:"FOLDER_WATCHER_PATH, default=.watch"`
}

type FilesystemConfig struct {
	Blacklist            []regexp.Regexp `env:"FILESYSTEM_BLACKLIST, default=(?i)\\.par2$"`
	FixFilenameThreshold float32         `env:"FILESYSTEM_FIX_FILENAME_THRESHOLD, default=0.2"`
}

type Config struct {
	Usenet         UsenetConfig
	Webdav         WebdavConfig
	Cache          CacheConfig
	ReadaheadCache ReadaheadCacheConfig
	Filesystem     FilesystemConfig
	FolderWatcher  FolderWatcherConfig
}
