package main

import (
	"context"
	"log/slog"
	"os"

	nntp "git.ruekov.eu/ruakij/nzbStreamer/internal/nntpClient"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbRecordFactory"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbStore/stubStore"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation/webdav"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/service/nzbService"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/trigger"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/trigger/folderWatcher"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/SimpleWebdavFilesystem"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskCache"

	gowebdav "github.com/emersion/go-webdav"

	"github.com/sethvargo/go-envconfig"
)

func main() {
	var err error = nil

	var c Config
	if err := envconfig.Process(context.Background(), &c); err != nil {
		slog.Error("Failed reading Env-variables for config", "error", err)
		os.Exit(1)
	}

	// Setup logging
	slog.SetLogLoggerLevel(c.Logging.Level)

	// Setup nntpClient
	nntpClient, err := nntp.SetupNntpClient(c.Usenet.Host, c.Usenet.Port, c.Usenet.Tls, c.Usenet.User, c.Usenet.Password, c.Usenet.MaxConn)
	if err != nil {
		slog.Error("Setup Usenet-Client failed", "error", err)
		os.Exit(1)
	}

	// Setup cache
	segmentCache, err := diskCache.NewCache(&diskCache.CacheOptions{
		CacheDir:             c.Cache.Path,
		MaxSize:              c.Cache.MaxSize,
		MaxSizeEvictBlocking: false,
	})
	if err != nil {
		slog.Error("Cache creation failed", "error", err)
		os.Exit(1)
	}

	filesystem := SimpleWebdavFilesystem.NewFS()

	factory := nzbRecordFactory.NewNzbFileFactory(segmentCache, nntpClient)
	factory.SetAdaptiveReadaheadCacheSettings(c.ReadaheadCache.AvgSpeedTime, c.ReadaheadCache.Time, c.ReadaheadCache.MinSize, c.ReadaheadCache.LowBuffer, c.ReadaheadCache.MaxSize)

	//store := folderStore.NewFolderStore()
	store := stubStore.NewStubStore()

	folderTrigger := folderWatcher.NewFolderWatcher(c.FolderWatcher.Path)

	service := nzbService.NewService(store, factory, filesystem, []trigger.Trigger{folderTrigger})
	service.SetBlacklist(c.Filesystem.Blacklist)
	service.SetPathFlatteningDepth(1)
	service.SetFilenameReplacementBelowLevensteinRatio(c.Filesystem.FixFilenameThreshold)

	if err = service.Init(); err != nil {
		panic(err)
	}
	folderTrigger.Init()

	// Serve webdav
	err = webdav.Listen(c.Webdav.Address, &gowebdav.Handler{
		FileSystem: filesystem,
	})
	if err != nil {
		panic(err)
	}
}
