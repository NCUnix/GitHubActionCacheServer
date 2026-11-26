package testutil

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/NCUnix/GitHubActionCacheServer/internal/config"
	"github.com/NCUnix/GitHubActionCacheServer/internal/db"
	"github.com/NCUnix/GitHubActionCacheServer/internal/ent"
	"github.com/NCUnix/GitHubActionCacheServer/internal/storage"
	"github.com/stretchr/testify/require"
)

func NewSQLiteClient(t testing.TB) (context.Context, *ent.Client) {
	t.Helper()

	ctx := context.Background()
	client, err := db.OpenAndMigrate(ctx, config.DBConfig{
		Driver:     db.DriverSQLite,
		SQLitePath: filepath.Join(t.TempDir(), "cache.db"),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})

	return ctx, client
}

func NewFilesystemAdapter(t testing.TB) *storage.FilesystemAdapter {
	t.Helper()

	filesystem, err := storage.NewFilesystemAdapter(t.TempDir())
	require.NoError(t, err)
	return filesystem
}

func NewSQLiteFilesystem(t testing.TB) (context.Context, *ent.Client, *storage.FilesystemAdapter) {
	t.Helper()

	ctx, client := NewSQLiteClient(t)
	return ctx, client, NewFilesystemAdapter(t)
}

// cacheService is the shutdown surface of *cache.Service; testutil cannot import cache because cache tests import testutil.
type cacheService interface {
	StopAcceptingMerges()
	WaitForMerges(context.Context) error
	ShutdownReaderLeaseReleaser(context.Context) error
}

// ShutdownCacheServiceOnCleanup stops the service's background writers before the database and temp dirs are torn down.
func ShutdownCacheServiceOnCleanup(tb testing.TB, service cacheService) {
	tb.Helper()

	tb.Cleanup(func() {
		service.StopAcceptingMerges()
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		// Cancel in-flight merges and wait for them to exit; the error only echoes the canceled context.
		_ = service.WaitForMerges(canceled)
		ctx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelShutdown()
		require.NoError(tb, service.ShutdownReaderLeaseReleaser(ctx))
	})
}
