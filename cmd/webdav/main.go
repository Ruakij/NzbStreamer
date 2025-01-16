package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	nntp "git.ruekov.eu/ruakij/nzbStreamer/internal/nntpClient"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbRecordFactory"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbStore/stubStore"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation/fusemount"
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
	context, cancel := context.WithCancel(context.Background())

	wg := start(context)
	signalHandler(cancel, wg)
}

func signalHandler(cancel context.CancelFunc, wg *sync.WaitGroup) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)
	for {
		sig := <-sigChan
		slog.Info("Received signal: %s\n", "signal", sig.String())
		cancel() // Cancel the context to stop goroutines

		// Wait for the WaitGroup with a maximum timeout of 3 seconds
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
			slog.Info("Clean shutdown.")
		case <-time.After(3 * time.Second):
			slog.Warn("Timeout reached, forcefully exiting.")
		}
		os.Exit(0)
	}
}

func start(ctx context.Context) (wg *sync.WaitGroup) {
	var err error = nil

	var c Config
	if err := envconfig.Process(ctx, &c); err != nil {
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

	// Setup Presenters
	presenters := make([]presentation.Presenter, 0, 2)
	// Webdav
	var webdavHandler *SimpleWebdavFilesystem.FS
	if c.Webdav.Address != "" {
		webdavHandler = SimpleWebdavFilesystem.NewFS()
		presenters = append(presenters, webdavHandler)
	}
	// Mount
	var mount *fusemount.FileSystem
	if c.Mount.Path != "" {
		mount = fusemount.Setup()
		presenters = append(presenters, mount)
	}

	// Setup services
	factory := nzbRecordFactory.NewNzbFileFactory(segmentCache, nntpClient)
	factory.SetAdaptiveReadaheadCacheSettings(c.ReadaheadCache.AvgSpeedTime, c.ReadaheadCache.Time, c.ReadaheadCache.MinSize, c.ReadaheadCache.LowBuffer, c.ReadaheadCache.MaxSize)

	//store := folderStore.NewFolderStore()
	store := stubStore.NewStubStore()

	folderTrigger := folderWatcher.NewFolderWatcher(c.FolderWatcher.Path)

	service := nzbService.NewService(store, factory, presenters, []trigger.Trigger{folderTrigger})
	service.SetBlacklist(c.Filesystem.Blacklist)
	service.SetPathFlatteningDepth(c.Filesystem.FlattenMaxDepth)
	service.SetFilenameReplacementBelowLevensteinRatio(c.Filesystem.FixFilenameThreshold)

	// Start services
	if err = service.Init(); err != nil {
		os.Exit(1)
	}
	folderTrigger.Init()

	// Start Presenters
	wg = &sync.WaitGroup{}
	// Webdav
	if c.Webdav.Address != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err = webdav.Listen(c.Webdav.Address, &gowebdav.Handler{
				FileSystem: webdavHandler,
			})
			if err != nil {
				slog.Error("Error in webdav", "error", err)
				os.Exit(1)
			}
			slog.Info("Webdav exited")
		}()
	}

	// Mount
	if c.Mount.Path != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := mount.Mount(ctx, c.Mount.Path, c.Mount.Options)
			if err != nil {
				slog.Error("Error in mount", "error", err)
				os.Exit(1)
			}
			slog.Info("Mount exited")
		}()
	}

	return
}
