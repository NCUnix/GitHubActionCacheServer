package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NCUnix/GitHubActionCacheServer/internal/auth"
	"github.com/NCUnix/GitHubActionCacheServer/internal/cache"
	"github.com/NCUnix/GitHubActionCacheServer/internal/cleanup"
	"github.com/NCUnix/GitHubActionCacheServer/internal/config"
	"github.com/NCUnix/GitHubActionCacheServer/internal/ent"
	"github.com/NCUnix/GitHubActionCacheServer/internal/httpapi"
	"github.com/NCUnix/GitHubActionCacheServer/internal/storage"
	"github.com/NCUnix/GitHubActionCacheServer/internal/testutil"
	"github.com/stretchr/testify/require"
)

func TestExternalS3BlockManifest(t *testing.T) {
	if _, ok := externalS3Config(); !ok {
		t.Skip("set E2E_S3_ENDPOINT_URL and E2E_S3_BUCKET")
	}
	_, client := testutil.NewSQLiteClient(t)
	runExternalS3BlockManifest(t, client)
}

func TestExternalPostgresS3BlockManifest(t *testing.T) {
	dbCfg, dbOK := externalPostgresConfig()
	_, s3OK := externalS3Config()
	if !dbOK || !s3OK {
		t.Skip("set PostgreSQL and S3 integration settings")
	}
	runExternalS3BlockManifest(t, openExternalDB(t, t.Context(), dbCfg))
}

func TestExternalMySQLS3BlockManifest(t *testing.T) {
	dbCfg, dbOK := externalMySQLConfig()
	_, s3OK := externalS3Config()
	if !dbOK || !s3OK {
		t.Skip("set MySQL and S3 integration settings")
	}
	runExternalS3BlockManifest(t, openExternalDB(t, t.Context(), dbCfg))
}

func TestExternalMySQLS3ConcurrentManifestUploads(t *testing.T) {
	dbCfg, dbOK := externalMySQLConfig()
	storageCfg, s3OK := externalS3Config()
	if !dbOK || !s3OK {
		t.Skip("set MySQL and S3 integration settings")
	}
	ctx := t.Context()
	client := openExternalDB(t, ctx, dbCfg)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	storageCfg.S3KeyPrefix = uniqueIntegrationCacheKey("concurrent-s3-manifests")
	ensureExternalS3Bucket(t, ctx, storageCfg)
	adapter, err := storage.NewS3Adapter(ctx, storageCfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, adapter.Clear(context.Background())) })

	service := newTestCacheService(t, cache.Options{DB: client, Storage: adapter})
	service.StopAcceptingMerges()
	scope := auth.CacheScope{RepoID: "123", Scopes: []auth.Scope{{Scope: "refs/heads/main", Permission: 3}}}
	const uploads = 32
	const version = "v1"
	const blockID = "block"
	keys := make([]string, uploads)
	sessions := make([]*cache.CreateUploadResult, uploads)
	for index := range sessions {
		keys[index] = fmt.Sprintf("cache-%d", index)
		session, err := service.CreateUpload(ctx, keys[index], version, scope)
		require.NoError(t, err)
		sessions[index] = session
	}

	runConcurrent := func(operation func(index int, uploadID int64) error) {
		t.Helper()
		start := make(chan struct{})
		failures := make([]error, uploads)
		var workers sync.WaitGroup
		for index, session := range sessions {
			workers.Go(func() {
				<-start
				failures[index] = operation(index, session.UploadID)
			})
		}
		close(start)
		workers.Wait()
		for index, err := range failures {
			require.NoError(t, err, "upload %d", index)
		}
	}
	runConcurrent(func(index int, uploadID int64) error {
		return service.UploadBlock(ctx, uploadID, blockID, strings.NewReader(keys[index]))
	})
	runConcurrent(func(_ int, uploadID int64) error {
		return service.CommitBlockList(ctx, uploadID, []string{blockID})
	})
	for _, key := range keys {
		_, err := service.CompleteUpload(ctx, key, version, scope)
		require.NoError(t, err)
		match, err := service.GetCacheEntryWithDownloadURL(ctx, []string{key}, version, scope, func(id string) string { return id })
		require.NoError(t, err)
		require.NotNil(t, match)
		restored, err := service.Download(ctx, match.CacheEntry.ID)
		require.NoError(t, err)
		data, err := io.ReadAll(restored)
		require.NoError(t, restored.Close())
		require.NoError(t, err)
		require.Equal(t, key, string(data))
	}
}

func runExternalS3BlockManifest(t *testing.T, client *ent.Client) {
	t.Helper()

	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	storageCfg, ok := externalS3Config()
	require.True(t, ok)
	storageCfg.S3KeyPrefix = uniqueIntegrationCacheKey("s3-block-manifest")
	ensureExternalS3Bucket(t, t.Context(), storageCfg)
	adapter, err := storage.NewS3Adapter(t.Context(), storageCfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, adapter.Clear(context.Background())) })

	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Cache.DownloadURLSigningSecret = "integration-test-secret"
	newReplica := func() *httptest.Server {
		service := newTestCacheService(t, cache.Options{
			DB:      client,
			Storage: adapter,
		})
		service.StopAcceptingMerges()
		router := newRouterWithCleanup(t, cfg, httpapi.Dependencies{
			DB:       client,
			Storage:  adapter,
			Cache:    service,
			Verifier: newSkipVerifier(t),
		})
		server := httptest.NewServer(router)
		t.Cleanup(server.Close)
		return server
	}
	writer, reader := newReplica(), newReplica()

	token := actionsToken(t)
	createBody := cacheBody(uniqueIntegrationCacheKey("manifest"))
	created := postServerJSON[createCacheResponse](t, writer, createCacheEntryPath, token, createBody)
	require.True(t, created.OK)
	uploadURL := mustParseURL(t, created.SignedUploadURL)
	uploadURL.Host = mustParseURL(t, reader.URL).Host
	const firstBlock = "b3BhcXVlLWJsb2NrLWZpcnN0"
	const secondBlock = "b3BhcXVlLWJsb2NrLXNlY29uZA=="
	putBlock := func(id, contents string) {
		partURL := *uploadURL
		query := partURL.Query()
		query.Set("comp", "block")
		query.Set("blockid", id)
		partURL.RawQuery = query.Encode()
		putServerBody(t, reader.Client(), partURL.String(), []byte(contents))
	}

	putBlock(secondBlock, "world")
	putBlock(firstBlock, "stale")
	putBlock(firstBlock, "hello ")
	commitBlockList(t, reader.Client(), uploadURL.String(), []string{firstBlock, secondBlock, firstBlock})
	putBlock(firstBlock, "uncommitted")
	finalized := postServerJSON[finalizeCacheResponse](t, reader, finalizeCacheEntryPath, token, createBody)
	require.True(t, finalized.OK)

	location, err := client.StorageLocation.Query().Only(t.Context())
	require.NoError(t, err)
	exists, err := adapter.ObjectExists(t.Context(), storage.PartsFolder(location.FolderName)+"/0")
	require.NoError(t, err)
	require.False(t, exists, "manifest commit must not copy blocks to numbered keys")

	sweeper := cleanup.NewService(cleanup.Options{
		DB:      client,
		Storage: adapter,
		Config:  config.CleanupConfig{OrphanedStorageGracePeriod: time.Hour},
		Now:     func() time.Time { return time.Now().Add(2 * time.Hour) },
	})
	deleted, err := sweeper.RunManifestCandidates(t.Context())
	require.NoError(t, err)
	require.Equal(t, 2, deleted)

	matched := postServerJSON[matchCacheResponse](t, reader, getCacheEntryDownloadPath, token, createBody)
	require.True(t, matched.OK)
	const restored = "hello worldhello "
	require.Equal(t, []byte(restored), getServerBody(t, reader.Client(), matched.SignedDownloadURL))

	rangeRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, matched.SignedDownloadURL, nil)
	require.NoError(t, err)
	rangeRequest.Header.Set("x-ms-range", "bytes=3-12")
	rangeResponse, err := reader.Client().Do(rangeRequest)
	require.NoError(t, err)
	defer rangeResponse.Body.Close()
	require.Equal(t, http.StatusPartialContent, rangeResponse.StatusCode)
	require.Equal(t, fmt.Sprintf("bytes 3-12/%d", len(restored)), rangeResponse.Header.Get("Content-Range"))
	ranged, err := io.ReadAll(rangeResponse.Body)
	require.NoError(t, err)
	require.Equal(t, restored[3:13], string(ranged))
}
