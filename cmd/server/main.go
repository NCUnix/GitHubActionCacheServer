package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/NCUnix/GitHubActionCacheServer/internal/auth"
	"github.com/NCUnix/GitHubActionCacheServer/internal/cache"
	"github.com/NCUnix/GitHubActionCacheServer/internal/cleanup"
	"github.com/NCUnix/GitHubActionCacheServer/internal/config"
	"github.com/NCUnix/GitHubActionCacheServer/internal/db"
	"github.com/NCUnix/GitHubActionCacheServer/internal/dbclock"
	"github.com/NCUnix/GitHubActionCacheServer/internal/dblock"
	"github.com/NCUnix/GitHubActionCacheServer/internal/httpapi"
	"github.com/NCUnix/GitHubActionCacheServer/internal/metrics"
	"github.com/NCUnix/GitHubActionCacheServer/internal/storage"
	"github.com/NCUnix/GitHubActionCacheServer/internal/storagecapacity"
	"github.com/NCUnix/GitHubActionCacheServer/internal/storagelifecycle"
	"github.com/NCUnix/GitHubActionCacheServer/internal/storagereconcile"
	"github.com/rs/zerolog"
)

const (
	shutdownTimeout         = 60 * time.Second
	serverReadHeaderTimeout = 30 * time.Second
	serverIdleTimeout       = 5 * time.Minute
)

func main() {
	cfg, err := config.Load()
	logLevel := zerolog.InfoLevel
	if cfg.Debug {
		logLevel = zerolog.DebugLevel
	}
	logger := zerolog.New(os.Stdout).Level(logLevel).With().Timestamp().Logger()
	if err != nil {
		logger.Fatal().Err(err).Msg("configuration load failed")
	}
	if cfg.Cache.DownloadURLSigningSecret == "" {
		logger.Warn().
			Str("environment_variable", "DOWNLOAD_URL_SIGNING_SECRET").
			Msg("URL signing secret is unset; using a random per-process key, so issued upload/download URLs will fail after restart or on another replica")
	}
	if cfg.Server.APIBaseURL == "" {
		logger.Warn().
			Str("environment_variable", "API_BASE_URL").
			Msg("public base URL is unset; generated URLs are derived from request headers, set this explicitly in production")
	}

	dbClient, err := db.OpenAndMigrate(logger.WithContext(context.Background()), cfg.DB)
	if err != nil {
		logger.Fatal().Err(err).Msg("database initialization failed")
	}
	defer func() {
		if err := dbClient.Close(); err != nil {
			logger.Error().Err(err).Msg("database close failed")
		}
	}()
	clock := dbclock.New(dbClient, cfg.DB.Driver, logger)
	if err := clock.Sync(context.Background()); err != nil {
		logger.Fatal().Err(err).Msg("database clock sync failed")
	}
	logger.Info().Dur("offset", clock.Offset()).Msg("database clock synchronized")

	storageAdapter, err := storage.NewAdapter(context.Background(), cfg.Storage)
	if err != nil {
		logger.Fatal().Err(err).Msg("storage initialization failed")
	}
	metricsRegistry := metrics.New(dbClient)
	lifecycleService := storagelifecycle.NewWithOptions(dbClient, storagelifecycle.Options{DirectDownloadURLTTL: cfg.Cache.DirectDownloadURLTTL, Now: clock.Now})
	var jobLocker *dblock.Locker
	// SQLite is single-instance, and a held lock transaction would occupy its only connection and deadlock.
	if cfg.DB.Driver != db.DriverSQLite {
		jobLocker = dblock.New(dbClient)
	}
	capacityService := storagecapacity.NewService(storagecapacity.Options{
		DB:        dbClient,
		Storage:   storageAdapter,
		Config:    cfg.Cache,
		Lifecycle: lifecycleService,
		Locker:    jobLocker,
		Logger:    &logger,
	})
	cacheService := cache.NewService(cache.Options{
		DB:                    dbClient,
		Storage:               storageAdapter,
		EnableDirectDownloads: cfg.Cache.EnableDirectDownloads,
		MergeConcurrency:      cfg.Cache.MergeConcurrency,
		Lifecycle:             lifecycleService,
		Logger:                &logger,
		Capacity:              capacityService,
		Now:                   clock.Now,
	})

	backgroundCtx, backgroundCancel := context.WithCancel(context.Background())
	defer backgroundCancel()

	verifier, err := auth.NewVerifier(backgroundCtx, auth.Options{
		Issuer:         cfg.Auth.TokenIssuer,
		JWKSURL:        cfg.Auth.TokenJWKSURL,
		Audience:       cfg.Auth.TokenAudience,
		SkipValidation: cfg.Auth.SkipTokenValidation,
		Logger:         &logger,
	})
	if err != nil {
		logger.Fatal().Err(err).Msg("token verifier initialization failed")
	}
	if cfg.Auth.SkipTokenValidation {
		logger.Warn().Str("environment_variable", "SKIP_TOKEN_VALIDATION").Msg("token validation is disabled; do not use in production")
	}

	storageSizeReady := make(chan struct{})
	var backgroundServices sync.WaitGroup
	backgroundServices.Go(func() {
		clock.Run(backgroundCtx)
	})
	backgroundServices.Go(func() {
		storagereconcile.Run(backgroundCtx, storagereconcile.Options{
			DB:        dbClient,
			Storage:   storageAdapter,
			Lifecycle: lifecycleService,
			Logger:    &logger,
		}, storageSizeReady)
	})
	backgroundServices.Go(func() {
		capacityService.Run(backgroundCtx, storageSizeReady)
	})
	cleanupRunner := cleanup.NewRunner(cleanup.NewService(cleanup.Options{
		DB:        dbClient,
		Storage:   storageAdapter,
		Config:    cfg.Cleanup,
		Lifecycle: lifecycleService,
		Locker:    jobLocker,
		Logger:    &logger,
		Metrics:   metricsRegistry,
		Now:       clock.Now,
	}), logger, storageSizeReady)
	backgroundServices.Go(func() {
		cleanupRunner.Run(backgroundCtx)
	})
	backgroundDone := make(chan struct{})
	go func() {
		backgroundServices.Wait()
		close(backgroundDone)
	}()

	server := newHTTPServer(cfg.Server.Addr, httpapi.NewRouter(logger, cfg, httpapi.Dependencies{
		DB:        dbClient,
		Storage:   storageAdapter,
		Cache:     cacheService,
		Lifecycle: lifecycleService,
		Metrics:   metricsRegistry,
		Verifier:  verifier,
	}))

	errCh := make(chan error, 1)
	go func() {
		listener, err := net.Listen("tcp", server.Addr)
		if err != nil {
			errCh <- err
			return
		}
		logger.Info().Str("addr", server.Addr).Msg("server listening")
		errCh <- server.Serve(newStalledClientListener(listener, cfg.Server.DownloadIdleTimeout))
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var serveErr error
	serveResultReceived := false
	select {
	case <-ctx.Done():
		stop()
		logger.Info().Dur("timeout", shutdownTimeout).Msg("shutdown signal received")
	case serveErr = <-errCh:
		serveResultReceived = true
		if !errors.Is(serveErr, http.ErrServerClosed) {
			logger.Fatal().Err(serveErr).Msg("server failed")
		}
	}

	backgroundCancel()
	cacheService.StopAcceptingMerges()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	shutdownErrCh := make(chan error, 1)
	go func() {
		shutdownErrCh <- server.Shutdown(shutdownCtx)
	}()

	mergeErrCh := make(chan error, 1)
	go func() {
		mergeErrCh <- cacheService.WaitForMerges(shutdownCtx)
	}()

	backgroundErrCh := make(chan error, 1)
	go func() {
		backgroundErrCh <- waitForBackgroundServices(shutdownCtx, backgroundDone)
	}()

	shutdownErr := <-shutdownErrCh
	readerLeaseReleaseErr := cacheService.ShutdownReaderLeaseReleaser(shutdownCtx)
	mergeErr := <-mergeErrCh
	backgroundErr := <-backgroundErrCh
	if shutdownErr != nil {
		logger.Fatal().Err(shutdownErr).Msg("graceful shutdown failed")
	}
	if readerLeaseReleaseErr != nil {
		logger.Error().Err(readerLeaseReleaseErr).Msg("waiting for reader lease releases failed")
	}
	if mergeErr != nil {
		logger.Fatal().Err(mergeErr).Msg("waiting for in-flight merges failed")
	}
	if backgroundErr != nil {
		logger.Fatal().Err(backgroundErr).Msg("waiting for background services failed")
	}

	if !serveResultReceived {
		serveErr = <-errCh
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		logger.Fatal().Err(serveErr).Msg("server stopped with error")
	}

	logger.Info().Msg("server stopped")
}

func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		IdleTimeout:       serverIdleTimeout,
	}
}

func waitForBackgroundServices(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		select {
		case <-done:
			return nil
		default:
			return ctx.Err()
		}
	}
}
