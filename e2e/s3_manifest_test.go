package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
