package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/NCUnix/GitHubActionCacheServer/internal/cache"
	"github.com/NCUnix/GitHubActionCacheServer/internal/storage"
	"github.com/NCUnix/GitHubActionCacheServer/internal/storagelifecycle"
	"github.com/NCUnix/GitHubActionCacheServer/internal/testutil"
	"github.com/stretchr/testify/require"
)

func TestStorageLifecycleReaderLockFollowsComposeCapability(t *testing.T) {
	ctx, client, log := testutil.NewRecordingSQLiteClient(t)
	adapter := testutil.NewFilesystemAdapter(t)
	now := time.Now()
	clock := func() time.Time { return now }
	lifecycle := newStorageLifecycle(client, adapter, time.Minute, clock)
	service := cache.NewService(cache.Options{DB: client, Storage: adapter, Lifecycle: lifecycle, Now: clock})
	t.Cleanup(func() { require.NoError(t, service.ShutdownReaderLeaseReleaser(ctx)) })

	location := client.StorageLocation.Create().
		SetID("location").SetFolderName("folder").SetPartCount(2).SetSizeBytes(8).
		SetLastDownloadedAt(now.UnixMilli()).SetRecencyAt(now.UnixMilli()).SaveX(ctx)
	entry := client.CacheEntry.Create().
		SetID("entry").SetKey("key").SetVersion("version").SetScope("scope").SetRepoId("repo").
		SetUpdatedAt(now.UnixMilli()).SetLocation(location).SaveX(ctx)
	parts := storage.PartsFolder(location.FolderName)
	require.NoError(t, adapter.UploadStream(ctx, parts+"/0", strings.NewReader("abcd")))
	require.NoError(t, adapter.UploadStream(ctx, parts+"/1", strings.NewReader("efgh")))

	for _, test := range []struct {
		name   string
		ranges []cache.ByteRange
		want   string
	}{
		{name: "full", want: "abcdefgh"},
		{name: "cross-part range", ranges: []cache.ByteRange{cache.ClosedByteRange(2, 5)}, want: "cdef"},
	} {
		t.Run(test.name, func(t *testing.T) {
			log.Reset()
			var stream *cache.DownloadStream
			var err error
			if len(test.ranges) == 0 {
				stream, err = service.Download(ctx, entry.ID)
			} else {
				stream, err = service.DownloadRanged(ctx, entry.ID, test.ranges)
			}
			require.NoError(t, err)
			defer func() { require.NoError(t, stream.Close()) }()
			data, err := io.ReadAll(stream)
			require.NoError(t, err)
			require.Equal(t, test.want, string(data))
			statements := log.Lines()
			require.Contains(t, strings.Join(statements, "\n"), "INSERT INTO `storage_reader_leases`")
			require.Equal(t, -1, testutil.LocationLockIndex(statements, location.ID), "statements: %v", statements)
		})
	}

	t.Run("S3 locks the location", func(t *testing.T) {
		lifecycle := newStorageLifecycle(client, &storage.S3Adapter{}, time.Minute, clock)
		log.Reset()
		lease, err := lifecycle.AcquireReader(ctx, entry.ID, storagelifecycle.AcquireReaderOptions{})
		require.NoError(t, err)
		defer func() { require.NoError(t, lifecycle.ReleaseReader(ctx, lease.ID)) }()
		require.GreaterOrEqual(t, testutil.LocationLockIndex(log.Lines(), location.ID), 0)
	})
}

func TestHTTPServerTimeoutsDoNotLimitCacheTransfers(t *testing.T) {
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	server := newHTTPServer(":3000", handler)

	require.Equal(t, serverReadHeaderTimeout, server.ReadHeaderTimeout)
	require.Equal(t, serverIdleTimeout, server.IdleTimeout)
	require.Zero(t, server.ReadTimeout)
	require.Zero(t, server.WriteTimeout)
}

func TestWaitForBackgroundServicesPrefersCompletionWhenContextIsAlsoCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	close(done)

	require.NoError(t, waitForBackgroundServices(ctx, done))
}

func TestWaitForBackgroundServicesReturnsContextErrorWhileStillRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.ErrorIs(t, waitForBackgroundServices(ctx, make(chan struct{})), context.Canceled)
}
