package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/filehealth"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nntpclient"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbrecordfactory"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbstore/sqlstore"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation/fusemount"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation/webdav"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/service/nzbservice"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/trigger"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/trigger/folderwatcher"
	shutdownmanager "git.ruekov.eu/ruakij/nzbStreamer/pkg/ShutdownManager"
	timeoutaction "git.ruekov.eu/ruakij/nzbStreamer/pkg/ShutdownManager/timeoutAction"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskcache"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/adaptiveparallelmergerresource"
	gowebdav "github.com/emersion/go-webdav"
	"github.com/sethvargo/go-envconfig"
)

const ShutdownTimeout time.Duration = 3 * time.Second

func main() {
	sm, ctx := shutdownmanager.NewShutdownManager(ShutdownTimeout, timeoutaction.Exit1)

	start(ctx, sm)
	signalHandler(ctx, sm)
}

func signalHandler(ctx context.Context, sm *shutdownmanager.ShutdownManager) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-sigChan:
		slog.Info("Received signal", "signal", sig.String())
		sm.Shutdown()
	case <-ctx.Done():
		signal.Stop(sigChan)
		close(sigChan)
	}
}

func start(ctx context.Context, sm *shutdownmanager.ShutdownManager) {
	var err error

	var c Config
	if err := envconfig.Process(ctx, &c); err != nil {
		slog.Error("Failed reading Env-variables for config", "error", err)
		os.Exit(1)
	}

	// Setup logging
	slog.SetLogLoggerLevel(c.Logging.Level)

	// Setup nntpClient
	nntpClient := nntpclient.New(nntpclient.Config{
		Host:     c.Usenet.Host,
		Port:     c.Usenet.Port,
		TLS:      c.Usenet.TLS,
		User:     c.Usenet.User,
		Pass:     c.Usenet.Password,
		MaxConns: c.Usenet.MaxConn,
		Attempts: c.Usenet.MaxAttempts,
		Backoff:  c.Usenet.RetryBackoff,
		Timeout:  c.Usenet.Timeout,

		IdleTimeout: c.Usenet.IdleTimeout,
	})

	// Setup prefetch, sharing the connection budget across all open files
	prefetchMaxConn := c.Prefetch.MaxConn
	if prefetchMaxConn <= 0 {
		prefetchMaxConn = c.Usenet.MaxConn
	}
	adaptiveparallelmergerresource.SetPrefetch(prefetchMaxConn, c.Prefetch.Time, c.Prefetch.MinSegments, c.Prefetch.MaxSegments)

	// Setup cache
	segmentCache, err := diskcache.NewCache(&diskcache.CacheOptions{
		CacheDir:             c.Cache.Path,
		MaxSize:              c.Cache.MaxSize,
		MaxSizeEvictBlocking: false,
	})
	if err != nil {
		slog.Error("Cache creation failed", "error", err)
		os.Exit(1)
	}

	// Setup Presenters
	var presenters []presentation.Presenter
	// Webdav
	var webdavHandler *webdav.FS
	if c.Webdav.Address != "" {
		webdavHandler = webdav.NewFS()
		presenters = append(presenters, webdavHandler)
	}
	// Mount
	var mount *fusemount.FileSystem
	if c.Mount.Path != "" {
		mount = fusemount.Setup()
		presenters = append(presenters, mount)
	}

	// Setup services
	store, err := sqlstore.New(c.Metadata.Path)
	if err != nil {
		slog.Error("Metadata store creation failed", "error", err)
		os.Exit(1)
	}
	// start returns while the presenters keep running, so closing the store is a
	// shutdown step rather than a defer; it is what flushes the sizes the read
	// path has learned
	sm.AddService()
	go func() {
		defer sm.ServiceDone()
		<-ctx.Done()
		if err := store.Close(); err != nil {
			slog.Error("Failed closing metadata store", "error", err)
		}
	}()

	factory := nzbrecordfactory.NewNzbFileFactory(segmentCache, nntpClient.GetSegment, store)

	folderTrigger := folderwatcher.NewFolderWatcher(c.FolderWatcher.Path, c.FolderWatcher.Consume)

	// Setup health checker
	segmentCheckParallel := c.NzbConfig.SegmentCheckParallel
	if segmentCheckParallel <= 0 {
		segmentCheckParallel = c.Usenet.MaxConn
	}
	healthChecker := filehealth.NewDefaultChecker(filehealth.CheckerConfig{
		SegmentsPerFile: c.NzbConfig.SegmentsPerFile,
		MaxParallel:     segmentCheckParallel,
	}, nntpClient.SegmentExists)

	service := nzbservice.NewService(store, factory, presenters, []trigger.Trigger{folderTrigger}, healthChecker)
	service.SetBlacklist(c.Filesystem.Blacklist)
	service.SetNzbFileBlacklist(c.NzbConfig.FileBlacklist)
	service.SetPathFlatteningDepth(c.Filesystem.FlattenMaxDepth)
	service.SetFilenameReplacementBelowLevensteinRatio(c.Filesystem.FixFilenameThreshold)
	service.SetFilesHealthyThreshold(c.NzbConfig.FilesHealthyThreshold)

	// Start services
	if err = service.Init(); err != nil {
		os.Exit(1)
	}
	folderTrigger.Init()

	// Start Presenters
	// Webdav
	if c.Webdav.Address != "" {
		sm.AddService()
		go func() {
			defer sm.ServiceDone()

			var authConfig *webdav.BasicAuthConfig
			if c.Webdav.Username != "" {
				authConfig = &webdav.BasicAuthConfig{
					Username: c.Webdav.Username,
					Password: c.Webdav.Password,
				}
			}

			err = webdav.Listen(ctx, c.Webdav.Address, &gowebdav.Handler{
				FileSystem: webdavHandler,
			}, authConfig)
			if err != nil {
				slog.Error("Error in webdav", "error", err)
				os.Exit(1)
			}
			slog.Info("Webdav exited")
		}()
	}

	// Mount
	if c.Mount.Path != "" {
		sm.AddService()
		go func() {
			defer sm.ServiceDone()
			err := mount.Mount(ctx, c.Mount.Path, c.Mount.Options)
			if err != nil {
				slog.Error("Error in mount", "error", err)
				os.Exit(1)
			}
			slog.Info("Mount exited")
		}()
	}
}
