package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/clientapi/sabnzbd"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/clientapi/webui"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/filehealth"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/httpserver"
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

const prefetchQueueMarginDivisor = 4

// prefetchQueueMargin defaults to a quarter of the pool.
func prefetchQueueMargin(configured, conns int) int {
	if configured >= 0 {
		return configured
	}
	return conns / prefetchQueueMarginDivisor
}

// completeDir is the path a download client api reports as the folder finished
// downloads land in, which is the mount unless it is told otherwise. A client
// imports from it, so it has to be absolute and it has to be the path as that
// client sees it.
func completeDir(c Config) string {
	dir := c.Sabnzbd.CompleteDir
	if dir == "" {
		dir = c.Mount.Path
	}
	if dir == "" {
		slog.Warn("No completed-downloads path for the sabnzbd api; set MOUNT_PATH or SABNZBD_COMPLETE_DIR, or a client cannot import what it downloads")
		return ""
	}

	absolute, err := filepath.Abs(dir)
	if err != nil {
		slog.Warn("Failed making the completed-downloads path absolute", "path", dir, "error", err)
		return dir
	}
	return absolute
}

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

	// Setup prefetch, sharing the connections and the queue for them across all
	// open files
	prefetchMaxConn := c.Prefetch.MaxConn
	if prefetchMaxConn <= 0 {
		prefetchMaxConn = c.Usenet.MaxConn
	}
	adaptiveparallelmergerresource.SetPrefetch(adaptiveparallelmergerresource.PrefetchSettings{
		Concurrency: prefetchMaxConn,
		LeadTime:    c.Prefetch.Time,
		MinLead:     c.Prefetch.MinSegments,
		MaxLead:     c.Prefetch.MaxSegments,
		Queued:      nntpClient.Waiting,
		QueueMargin: prefetchQueueMargin(c.Prefetch.QueueMargin, c.Usenet.MaxConn),
	})

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
	webdavFS := webdav.NewFS(httpserver.WebdavPrefix)
	presenters = append(presenters, webdavFS)
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

	factory := nzbrecordfactory.NewNzbFileFactory(segmentCache, nntpClient.GetSegment, store, c.NzbConfig.ProbeSizeConvention)

	folderTrigger := folderwatcher.NewFolderWatcher(c.FolderWatcher.Path, c.FolderWatcher.Consume)

	// Setup health checker
	probeParallel := c.Probe.Parallel
	if probeParallel <= 0 {
		probeParallel = c.Usenet.MaxConn
	}
	healthChecker := filehealth.NewDefaultChecker(filehealth.CheckerConfig{
		InitialFilePercent:       c.Probe.InitialFilePercent,
		InitialFileMinSegments:   c.Probe.InitialFileMinSegments,
		InitialFileMaxSegments:   c.Probe.InitialFileMaxSegments,
		ExtensiveFilePercent:     c.Probe.ExtensiveFilePercent,
		ExtensiveFileMaxSegments: c.Probe.ExtensiveFileMaxSegments,
		MaxMissingPercent:        c.Probe.MaxMissingPercent,
		Par2Safety:               c.Probe.Par2Safety,
		UndecidedAccept:          c.Probe.UndecidedAccept,
		Confidence:               c.Probe.Confidence,
		MaxParallel:              probeParallel,
	}, nntpClient.SegmentExists)

	service := nzbservice.NewService(store, factory, presenters, []trigger.Trigger{folderTrigger}, healthChecker)
	service.SetBlacklist(c.Filesystem.Blacklist)
	service.SetNzbFileBlacklist(c.NzbConfig.FileBlacklist)
	service.SetPathFlatteningDepth(c.Filesystem.FlattenMaxDepth)
	service.SetFilenameReplacementBelowLevensteinRatio(c.Filesystem.FixFilenameThreshold)

	// Mount before the service restores its tree: an inode only takes children
	// once the filesystem it belongs to is mounted
	if c.Mount.Path != "" {
		if err = mount.Mount(c.Mount.Path, c.Mount.Options); err != nil {
			slog.Error("Mounting failed", "error", err)
			os.Exit(1)
		}
		sm.AddService()
		go func() {
			defer sm.ServiceDone()
			if err := mount.Serve(ctx); err != nil {
				slog.Error("Error in mount", "error", err)
				os.Exit(1)
			}
			slog.Info("Mount exited")
		}()
	}

	// Start services
	if err = service.Init(); err != nil {
		os.Exit(1)
	}
	folderTrigger.Init()

	// Http server: everything the process speaks on one address
	var webdavAuth *webdav.BasicAuthConfig
	if c.Webdav.Username != "" {
		webdavAuth = &webdav.BasicAuthConfig{
			Username: c.Webdav.Username,
			Password: c.Webdav.Password,
		}
	}

	mux := httpserver.NewMux(httpserver.Routes{
		WebUI: webui.NewHandler(service),
		Sabnzbd: sabnzbd.NewHandler(service, sabnzbd.Config{
			APIKey:      c.Sabnzbd.APIKey,
			CompleteDir: completeDir(c),
			Categories:  c.Sabnzbd.Categories,
		}),
		Webdav: webdav.BasicAuth(&gowebdav.Handler{FileSystem: webdavFS}, webdavAuth),
		Debug:  c.HTTP.Debug,
	})

	sm.AddService()
	go func() {
		defer sm.ServiceDone()

		if err := httpserver.Listen(ctx, c.HTTP.Address, mux); err != nil {
			slog.Error("Error in http server", "error", err)
			os.Exit(1)
		}
		slog.Info("Http server exited")
	}()
}
