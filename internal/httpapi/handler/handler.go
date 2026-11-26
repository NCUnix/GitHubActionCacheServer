package handler

import (
	"errors"
	"time"

	"github.com/NCUnix/GitHubActionCacheServer/internal/cache"
	"github.com/NCUnix/GitHubActionCacheServer/internal/config"
	"github.com/NCUnix/GitHubActionCacheServer/internal/ent"
	"github.com/NCUnix/GitHubActionCacheServer/internal/httpapi/downloadurl"
	"github.com/NCUnix/GitHubActionCacheServer/internal/httpapi/response"
	"github.com/NCUnix/GitHubActionCacheServer/internal/metrics"
	"github.com/NCUnix/GitHubActionCacheServer/internal/storage"
	"github.com/NCUnix/GitHubActionCacheServer/internal/storagelifecycle"
	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
)

const (
	fallbackDownloadURLTTL = 10 * time.Minute
	uploadURLTTL           = 24 * time.Hour
)

type Handler struct {
	cfg            config.Config
	cache          *cache.Service
	db             *ent.Client
	storage        storage.Adapter
	lifecycle      *storagelifecycle.Service
	downloadSigner *downloadurl.Signer
	uploadSigner   *downloadurl.Signer
	metrics        metrics.Recorder
	logger         zerolog.Logger
}

type Options struct {
	Config    config.Config
	Cache     *cache.Service
	DB        *ent.Client
	Storage   storage.Adapter
	Lifecycle *storagelifecycle.Service
	Metrics   metrics.Recorder
	Logger    *zerolog.Logger
}

func New(options Options) *Handler {
	lifecycle := options.Lifecycle
	if lifecycle == nil {
		lifecycle = storagelifecycle.New(options.DB)
	}
	logger := zerolog.Nop()
	if options.Logger != nil {
		logger = *options.Logger
	}
	metricsRecorder := options.Metrics
	if metricsRecorder == nil {
		metricsRecorder = metrics.NopRecorder()
	}
	return &Handler{
		cfg:            options.Config,
		cache:          options.Cache,
		db:             options.DB,
		storage:        options.Storage,
		lifecycle:      lifecycle,
		downloadSigner: downloadurl.New(options.Config.Cache.DownloadURLSigningSecret, fallbackDownloadURLTTL),
		uploadSigner:   downloadurl.New(options.Config.Cache.DownloadURLSigningSecret, uploadURLTTL),
		metrics:        metricsRecorder,
		logger:         logger,
	}
}

func (h *Handler) writeCacheError(c *gin.Context, err error) {
	code, message, expected := classifyCacheError(err)
	if !expected {
		response.InternalError(c, err)
		return
	}
	h.logExpectedCacheError(c, err)
	response.JSON(c, response.Error(code.HTTPStatus(), message))
}

func (h *Handler) writeTwirpCacheError(c *gin.Context, err error) {
	code, message, expected := classifyCacheError(err)
	if !expected {
		response.TwirpInternalError(c, err)
		return
	}
	h.logExpectedCacheError(c, err)
	response.JSON(c, response.TwirpError(code, message))
}

// classifyCacheError maps a cache service error to its public Twirp code and
// message; expected is false for internal failures whose cause must stay private.
func classifyCacheError(err error) (code response.TwirpCode, message string, expected bool) {
	switch {
	case errors.Is(err, cache.ErrNoWriteScope):
		return response.TwirpPermissionDenied, cache.ErrNoWriteScope.Error(), true
	case errors.Is(err, cache.ErrUploadAlreadyExists):
		return response.TwirpAlreadyExists, "cache entry with the same key, version, and scope already exists", true
	case errors.Is(err, cache.ErrUploadNotFound):
		return response.TwirpNotFound, cache.ErrUploadNotFound.Error(), true
	case errors.Is(err, cache.ErrCacheNotFound):
		return response.TwirpNotFound, cache.ErrCacheNotFound.Error(), true
	case errors.Is(err, cache.ErrPartCountMismatch):
		return response.TwirpInvalidArgument, cache.ErrPartCountMismatch.Error(), true
	case errors.Is(err, cache.ErrBlockListTooLarge):
		return response.TwirpInvalidArgument, cache.ErrBlockListTooLarge.Error(), true
	case errors.Is(err, cache.ErrNoPartsUploaded):
		return response.TwirpInvalidArgument, cache.ErrNoPartsUploaded.Error(), true
	default:
		return response.TwirpInternal, response.InternalServerErrorMessage, false
	}
}

func (h *Handler) logExpectedCacheError(c *gin.Context, err error) {
	h.logger.Debug().
		Err(err).
		Str("method", c.Request.Method).
		Str("path", c.Request.URL.Path).
		Msg("cache request rejected")
}
