package e2e

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/NCUnix/GitHubActionCacheServer/internal/auth"
	"github.com/NCUnix/GitHubActionCacheServer/internal/cache"
	"github.com/NCUnix/GitHubActionCacheServer/internal/cleanup"
	"github.com/NCUnix/GitHubActionCacheServer/internal/config"
	"github.com/NCUnix/GitHubActionCacheServer/internal/db"
	"github.com/NCUnix/GitHubActionCacheServer/internal/dbclock"
	"github.com/NCUnix/GitHubActionCacheServer/internal/dblock"
	"github.com/NCUnix/GitHubActionCacheServer/internal/ent"
	"github.com/NCUnix/GitHubActionCacheServer/internal/ent/storagereaderlease"
	"github.com/NCUnix/GitHubActionCacheServer/internal/ent/upload"
	"github.com/NCUnix/GitHubActionCacheServer/internal/httpapi"
	"github.com/NCUnix/GitHubActionCacheServer/internal/storage"
	"github.com/NCUnix/GitHubActionCacheServer/internal/storagecapacity"
	"github.com/NCUnix/GitHubActionCacheServer/internal/storagelifecycle"
	"github.com/NCUnix/GitHubActionCacheServer/internal/testutil"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/go-sql-driver/mysql"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

const externalS3CacheSize = 5*1024*1024 + 1024

const postgresMigrationLockKey = int64(2389201325396535817)

type externalMigrationResult struct {
	client *ent.Client
	err    error
}

func TestExternalPostgresFilesystemSaveAndRestore(t *testing.T) {
	dbCfg, ok := externalPostgresConfig()
	if !ok {
		t.Skip("set E2E_POSTGRES_URL to run PostgreSQL integration coverage")
	}

	ctx := context.Background()
	client := openExternalDB(t, ctx, dbCfg)
	filesystem := testutil.NewFilesystemAdapter(t)
	router := newExternalRouter(t, client, filesystem)

	runSaveRestoreFlow(t, router, uniqueIntegrationCacheKey("postgres"), "postgres-cache-content")
}

func TestExternalMySQLFilesystemSaveAndRestore(t *testing.T) {
	dbCfg, ok := externalMySQLConfig()
	if !ok {
		t.Skip("set E2E_MYSQL_HOST, E2E_MYSQL_DATABASE and E2E_MYSQL_USER to run MySQL integration coverage")
	}

	ctx := context.Background()
	client := openExternalDB(t, ctx, dbCfg)
	filesystem := testutil.NewFilesystemAdapter(t)
	router := newExternalRouter(t, client, filesystem)

	runSaveRestoreFlow(t, router, uniqueIntegrationCacheKey("mysql"), "mysql-cache-content")
}

func TestExternalMySQLUploadBlocksAtSameTimestamp(t *testing.T) {
	dbCfg, ok := externalMySQLConfig()
	if !ok {
		t.Skip("set E2E_MYSQL_HOST, E2E_MYSQL_DATABASE and E2E_MYSQL_USER to run MySQL integration coverage")
	}

	ctx := context.Background()
	client := openExternalDB(t, ctx, dbCfg)
	now := time.Now()
	service := newTestCacheService(t, cache.Options{
		DB:      client,
		Storage: testutil.NewFilesystemAdapter(t),
		Now:     func() time.Time { return now },
	})
	scope := auth.CacheScope{RepoID: "123", Scopes: []auth.Scope{{Scope: "refs/heads/main", Permission: 3}}}
	reservation, err := service.CreateUpload(ctx, uniqueIntegrationCacheKey("mysql"), defaultCacheEntryVersion, scope)
	require.NoError(t, err)
	require.NoError(t, service.UploadBlock(ctx, reservation.UploadID, "first", strings.NewReader("first")))
	require.NoError(t, service.UploadBlock(ctx, reservation.UploadID, "second", strings.NewReader("second")))
}

func TestExternalPostgresReaderLeaseForeignKeyIsClassified(t *testing.T) {
	dbCfg, ok := externalPostgresConfig()
	if !ok {
		t.Skip("set E2E_POSTGRES_URL to run PostgreSQL integration coverage")
	}

	ctx := context.Background()
	client := openExternalDB(t, ctx, dbCfg)

	_, err := client.StorageReaderLease.Create().
		SetID("orphan-lease").
		SetStorageLocationId("missing-location").
		SetScope(storagereaderlease.ScopeStorage).
		SetExpiresAt(time.Now().Add(time.Minute).UnixMilli()).
		Save(ctx)
	require.True(t, ent.IsConstraintError(err))
}

func TestExternalMySQLReaderLeaseForeignKeyIsClassified(t *testing.T) {
	dbCfg, ok := externalMySQLConfig()
	if !ok {
		t.Skip("set E2E_MYSQL_HOST, E2E_MYSQL_DATABASE and E2E_MYSQL_USER to run MySQL integration coverage")
	}

	ctx := context.Background()
	client := openExternalDB(t, ctx, dbCfg)

	_, err := client.StorageReaderLease.Create().
		SetID("orphan-lease").
		SetStorageLocationId("missing-location").
		SetScope(storagereaderlease.ScopeStorage).
		SetExpiresAt(time.Now().Add(time.Minute).UnixMilli()).
		Save(ctx)
	require.True(t, ent.IsConstraintError(err))
}

func TestExternalPostgresSchemaMigrationSerialization(t *testing.T) {
	dbCfg, ok := externalPostgresConfig()
	if !ok {
		t.Skip("set E2E_POSTGRES_URL to run PostgreSQL migration serialization coverage")
	}

	ctx := context.Background()
	prepareExternalSizeColumnMigration(t, ctx, dbCfg)
	blockerDB := openExternalSQLDB(t, ctx, dbCfg)
	t.Cleanup(func() { require.NoError(t, blockerDB.Close()) })
	blockerTx, err := blockerDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = blockerTx.Rollback() }()
	_, err = blockerTx.ExecContext(ctx, "select pg_advisory_xact_lock($1)", postgresMigrationLockKey)
	require.NoError(t, err)

	resultCh := startExternalMigration(ctx, dbCfg)
	requireMigrationStillWaiting(t, resultCh)
	require.NoError(t, blockerTx.Commit())
	requireExternalMigrationSuccess(t, resultCh)
	requireExternalSizeColumn(t, ctx, dbCfg)
}

func TestExternalPostgresSchemaMigrationCommitsDDLBeforeBackfill(t *testing.T) {
	dbCfg, ok := externalPostgresConfig()
	if !ok {
		t.Skip("set E2E_POSTGRES_URL to run PostgreSQL migration lock scope coverage")
	}

	ctx := context.Background()
	client := openExternalDB(t, ctx, dbCfg)
	locationID := uniqueIntegrationCacheKey("backfill-location")
	client.StorageLocation.Create().
		SetID(locationID).
		SetFolderName(locationID + "-folder").
		SetPartCount(1).
		SetRecencyAt(0).
		SaveX(ctx)

	t.Cleanup(func() {
		restored, err := db.OpenAndMigrate(context.Background(), dbCfg)
		require.NoError(t, err)
		if restored != nil {
			require.NoError(t, restored.Close())
		}
	})
	sqlDB := openExternalSQLDB(t, ctx, dbCfg)
	_, err := sqlDB.ExecContext(ctx, `alter table uploads drop column "lastPartUploadedAt"`)
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())

	blockerDB := openExternalSQLDB(t, ctx, dbCfg)
	t.Cleanup(func() { require.NoError(t, blockerDB.Close()) })
	blockerTx, err := blockerDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = blockerTx.Rollback() }()
	_, err = blockerTx.ExecContext(ctx, "select 1 from storage_locations where id = $1 for update", locationID)
	require.NoError(t, err)

	resultCh := startExternalMigration(ctx, dbCfg)
	requireExternalRowsEventually(t, ctx, blockerDB,
		"backfill did not block on the storage location row lock",
		"select count(*) from pg_stat_activity where datname = current_database() and wait_event_type = 'Lock' and query ilike 'update storage_locations%'",
	)

	var columnExists bool
	require.NoError(t, blockerDB.QueryRowContext(ctx, `select exists(
		select 1 from information_schema.columns
		where table_schema = current_schema()
			and table_name = 'uploads'
			and column_name = 'lastPartUploadedAt'
	)`).Scan(&columnExists))
	require.True(t, columnExists, "schema DDL must be committed before the backfill runs")

	require.NoError(t, blockerTx.Commit())
	requireExternalMigrationSuccess(t, resultCh)
}

func TestExternalMySQLSchemaMigrationSerialization(t *testing.T) {
	dbCfg, ok := externalMySQLConfig()
	if !ok {
		t.Skip("set E2E_MYSQL_HOST, E2E_MYSQL_DATABASE and E2E_MYSQL_USER to run MySQL migration serialization coverage")
	}

	ctx := context.Background()
	prepareExternalSizeColumnMigration(t, ctx, dbCfg)
	blockerDB := openExternalSQLDB(t, ctx, dbCfg)
	t.Cleanup(func() { require.NoError(t, blockerDB.Close()) })
	blockerConn, err := blockerDB.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blockerConn.Close()) })
	lockName := externalMySQLMigrationLockName(dbCfg.MySQLDatabase)
	var acquired sql.NullInt64
	require.NoError(t, blockerConn.QueryRowContext(ctx, "select get_lock(?, 0)", lockName).Scan(&acquired))
	require.True(t, acquired.Valid)
	require.Equal(t, int64(1), acquired.Int64)
	lockHeld := true
	defer func() {
		if lockHeld {
			_, _ = blockerConn.ExecContext(context.Background(), "select release_lock(?)", lockName)
		}
	}()

	resultCh := startExternalMigration(ctx, dbCfg)
	requireMigrationStillWaiting(t, resultCh)
	var released sql.NullInt64
	require.NoError(t, blockerConn.QueryRowContext(ctx, "select release_lock(?)", lockName).Scan(&released))
	require.True(t, released.Valid)
	require.Equal(t, int64(1), released.Int64)
	lockHeld = false
	requireExternalMigrationSuccess(t, resultCh)
	requireExternalSizeColumn(t, ctx, dbCfg)
}

func TestExternalMySQLSchemaMigrationRunsOnLockSession(t *testing.T) {
	dbCfg, ok := externalMySQLConfig()
	if !ok {
		t.Skip("set E2E_MYSQL_HOST, E2E_MYSQL_DATABASE and E2E_MYSQL_USER to run MySQL migration lock session coverage")
	}

	ctx := context.Background()
	prepareExternalSizeColumnMigration(t, ctx, dbCfg)
	blockerDB := openExternalSQLDB(t, ctx, dbCfg)
	t.Cleanup(func() { require.NoError(t, blockerDB.Close()) })
	blockerTx, err := blockerDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = blockerTx.Rollback() }()
	_, err = blockerTx.ExecContext(ctx, "select 1 from storage_locations limit 1")
	require.NoError(t, err)

	resultCh := startExternalMigration(ctx, dbCfg)
	requireExternalRowsEventually(t, ctx, blockerDB,
		"ALTER TABLE did not run on the session holding the migration lock",
		"select count(*) from information_schema.processlist where id = is_used_lock(?) and lower(info) like 'alter table%'",
		externalMySQLMigrationLockName(dbCfg.MySQLDatabase),
	)

	require.NoError(t, blockerTx.Commit())
	requireExternalMigrationSuccess(t, resultCh)
	requireExternalSizeColumn(t, ctx, dbCfg)
}

func TestSQLiteCacheKeyMatching(t *testing.T) {
	app := newTestApp(t)
	runCacheKeyMatching(t, app.db, app.router)
}

func TestExternalPostgresCacheKeyMatching(t *testing.T) {
	dbCfg, ok := externalPostgresConfig()
	if !ok {
		t.Skip("set E2E_POSTGRES_URL to run PostgreSQL integration coverage")
	}

	ctx := context.Background()
	client := openExternalDB(t, ctx, dbCfg)
	router := newExternalRouter(t, client, testutil.NewFilesystemAdapter(t))
	runCacheKeyMatching(t, client, router)
}

func TestExternalMySQLCacheKeyMatching(t *testing.T) {
	dbCfg, ok := externalMySQLConfig()
	if !ok {
		t.Skip("set E2E_MYSQL_HOST, E2E_MYSQL_DATABASE and E2E_MYSQL_USER to run MySQL integration coverage")
	}

	ctx := context.Background()
	client := openExternalDB(t, ctx, dbCfg)
	useCaseInsensitiveMySQLCacheKeyCollation(t, ctx, dbCfg)
	router := newExternalRouter(t, client, testutil.NewFilesystemAdapter(t))
	runCacheKeyMatching(t, client, router)
}

func TestExternalPostgresStorageSizeMigration(t *testing.T) {
	dbCfg, ok := externalPostgresConfig()
	if !ok {
		t.Skip("set E2E_POSTGRES_URL to run PostgreSQL integration coverage")
	}

	runExternalStorageSizeMigration(t, dbCfg)
}

func TestExternalMySQLStorageSizeMigration(t *testing.T) {
	dbCfg, ok := externalMySQLConfig()
	if !ok {
		t.Skip("set E2E_MYSQL_HOST, E2E_MYSQL_DATABASE and E2E_MYSQL_USER to run MySQL integration coverage")
	}

	runExternalStorageSizeMigration(t, dbCfg)
}

func TestExternalPostgresCapacityEviction(t *testing.T) {
	dbCfg, ok := externalPostgresConfig()
	if !ok {
		t.Skip("set E2E_POSTGRES_URL to run PostgreSQL integration coverage")
	}

	runExternalCapacityEviction(t, dbCfg)
}

func TestExternalMySQLCapacityEviction(t *testing.T) {
	dbCfg, ok := externalMySQLConfig()
	if !ok {
		t.Skip("set E2E_MYSQL_HOST, E2E_MYSQL_DATABASE and E2E_MYSQL_USER to run MySQL integration coverage")
	}

	runExternalCapacityEviction(t, dbCfg)
}

func TestExternalPostgresCapacityEvictionSkipsWhileLocked(t *testing.T) {
	dbCfg, ok := externalPostgresConfig()
	if !ok {
		t.Skip("set E2E_POSTGRES_URL to run PostgreSQL integration coverage")
	}

	runExternalCapacityEvictionSkipsWhileLocked(t, dbCfg)
}

func TestExternalMySQLCapacityEvictionSkipsWhileLocked(t *testing.T) {
	dbCfg, ok := externalMySQLConfig()
	if !ok {
		t.Skip("set E2E_MYSQL_HOST, E2E_MYSQL_DATABASE and E2E_MYSQL_USER to run MySQL integration coverage")
	}

	runExternalCapacityEvictionSkipsWhileLocked(t, dbCfg)
}

func TestExternalPostgresJobLockExclusion(t *testing.T) {
	dbCfg, ok := externalPostgresConfig()
	if !ok {
		t.Skip("set E2E_POSTGRES_URL to run PostgreSQL integration coverage")
	}

	runExternalJobLockExclusion(t, dbCfg)
}

func TestExternalMySQLJobLockExclusion(t *testing.T) {
	dbCfg, ok := externalMySQLConfig()
	if !ok {
		t.Skip("set E2E_MYSQL_HOST, E2E_MYSQL_DATABASE and E2E_MYSQL_USER to run MySQL integration coverage")
	}

	runExternalJobLockExclusion(t, dbCfg)
}

func TestExternalPostgresJobLockLoss(t *testing.T) {
	dbCfg, ok := externalPostgresConfig()
	if !ok {
		t.Skip("set E2E_POSTGRES_URL to run PostgreSQL integration coverage")
	}

	runExternalPostgresJobLockLoss(t, dbCfg)
}

func TestExternalMySQLJobLockLoss(t *testing.T) {
	dbCfg, ok := externalMySQLConfig()
	if !ok {
		t.Skip("set E2E_MYSQL_HOST, E2E_MYSQL_DATABASE and E2E_MYSQL_USER to run MySQL integration coverage")
	}

	runExternalMySQLJobLockLoss(t, dbCfg)
}

func TestExternalPostgresJobClaimRun(t *testing.T) {
	dbCfg, ok := externalPostgresConfig()
	if !ok {
		t.Skip("set E2E_POSTGRES_URL to run PostgreSQL integration coverage")
	}

	runExternalJobClaimRun(t, dbCfg)
}

func TestExternalMySQLJobClaimRun(t *testing.T) {
	dbCfg, ok := externalMySQLConfig()
	if !ok {
		t.Skip("set E2E_MYSQL_HOST, E2E_MYSQL_DATABASE and E2E_MYSQL_USER to run MySQL integration coverage")
	}

	runExternalJobClaimRun(t, dbCfg)
}

func TestExternalPostgresCleanupRetention(t *testing.T) {
	dbCfg, ok := externalPostgresConfig()
	if !ok {
		t.Skip("set E2E_POSTGRES_URL to run PostgreSQL integration coverage")
	}

	runExternalCleanupRetention(t, dbCfg)
}

func TestExternalMySQLCleanupRetention(t *testing.T) {
	dbCfg, ok := externalMySQLConfig()
	if !ok {
		t.Skip("set E2E_MYSQL_HOST, E2E_MYSQL_DATABASE and E2E_MYSQL_USER to run MySQL integration coverage")
	}

	runExternalCleanupRetention(t, dbCfg)
}

func TestExternalPostgresDatabaseClockSync(t *testing.T) {
	dbCfg, ok := externalPostgresConfig()
	if !ok {
		t.Skip("set E2E_POSTGRES_URL to run PostgreSQL integration coverage")
	}

	runExternalDatabaseClockSync(t, dbCfg)
}

func TestExternalMySQLDatabaseClockSync(t *testing.T) {
	dbCfg, ok := externalMySQLConfig()
	if !ok {
		t.Skip("set E2E_MYSQL_HOST, E2E_MYSQL_DATABASE and E2E_MYSQL_USER to run MySQL integration coverage")
	}

	runExternalDatabaseClockSync(t, dbCfg)
}

func TestExternalS3SaveAndRestore(t *testing.T) {
	storageCfg, ok := externalS3Config()
	if !ok {
		t.Skip("set E2E_S3_ENDPOINT_URL and E2E_S3_BUCKET to run S3 integration coverage")
	}
	if os.Getenv("AWS_EC2_METADATA_DISABLED") == "" {
		t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	}

	ctx, client := testutil.NewSQLiteClient(t)
	ensureExternalS3Bucket(t, ctx, storageCfg)
	s3Storage, err := storage.NewS3Adapter(ctx, storageCfg)
	require.NoError(t, err)
	require.NoError(t, s3Storage.Clear(ctx))
	t.Cleanup(func() {
		require.NoError(t, s3Storage.Clear(context.Background()))
	})

	router := newExternalRouter(t, client, s3Storage)
	runSaveRestoreFlow(t, router, uniqueIntegrationCacheKey("s3"), strings.Repeat("s", externalS3CacheSize))
}

func runSaveRestoreFlow(t *testing.T, router http.Handler, key string, content string) {
	t.Helper()

	token := actionsToken(t)
	createBody := cacheBody(key)
	uploadURL := createCacheEntry(t, router, token, createBody)
	uploadWholeCache(t, router, uploadURL, content)
	finalizeCacheEntry(t, router, token, createBody)

	matchResponse := matchCacheEntry(t, router, token, map[string]any{
		"key":          key,
		"restore_keys": []string{strings.TrimSuffix(key, "-missing")},
		"version":      defaultCacheEntryVersion,
	})
	require.Equal(t, key, matchResponse.MatchedKey)
	require.Equal(t, content, downloadCache(t, router, parseSignedURL(t, matchResponse.SignedDownloadURL)))
}

func runCacheKeyMatching(t *testing.T, client *ent.Client, router http.Handler) {
	t.Helper()

	ctx := context.Background()
	token := actionsToken(t)
	upperKey := "Case-Key"
	upperBody := cacheBody(upperKey)
	uploadURL := createCacheEntry(t, router, token, upperBody)
	uploadWholeCache(t, router, uploadURL, "upper-v1")
	finalizeCacheEntry(t, router, token, upperBody)

	// A case-only variant must not confuse the exact-match predicates on a
	// case-insensitive collation: reserve refuses the exact live key without
	// false-hitting the variant in either direction.
	newer := time.Now().Add(time.Hour).UnixMilli()
	createCacheKeyMatchEntry(ctx, client, "lower-variant", "case-key", newer)

	rerun := postJSON(t, router, createCacheEntryPath, token, upperBody)
	require.Equal(t, http.StatusConflict, rerun.Code)

	caseVariant := createCacheEntryResponse(t, router, token, cacheBody("CASE-KEY"))
	require.True(t, caseVariant.OK)
	_, err := client.Upload.Delete().Where(upload.Key("CASE-KEY")).Exec(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, client.CacheEntry.Query().CountX(ctx))

	service := newTestCacheService(t, cache.Options{DB: client})
	scope := auth.CacheScope{
		RepoID: "123",
		Scopes: []auth.Scope{{Scope: "refs/heads/main", Permission: 3}},
	}

	for _, test := range []struct {
		name string
		keys []string
		want string
	}{
		{name: "upper exact", keys: []string{upperKey}, want: upperKey},
		{name: "lower exact", keys: []string{"case-key"}, want: "case-key"},
		{name: "case-only miss", keys: []string{"CASE-KEY"}},
		{name: "upper prefix ignores newer lower variant", keys: []string{"missing", "Case-"}, want: upperKey},
	} {
		t.Run(test.name, func(t *testing.T) {
			match, err := service.MatchCacheEntry(ctx, test.keys, defaultCacheEntryVersion, scope)
			require.NoError(t, err)
			if test.want == "" {
				require.Nil(t, match)
				return
			}
			require.NotNil(t, match)
			require.Equal(t, test.want, match.Key)
		})
	}

	for i, test := range []struct {
		name   string
		prefix string
		match  string
		decoy  string
	}{
		{name: "percent", prefix: "literal%", match: "literal%-match", decoy: "literalX-decoy"},
		{name: "underscore", prefix: "literal_", match: "literal_-match", decoy: "literalX_other"},
		{name: "backslash", prefix: `literal\`, match: `literal\-match`, decoy: "literalX-slash"},
	} {
		createCacheKeyMatchEntry(ctx, client, fmt.Sprintf("literal-match-%d", i), test.match, newer+int64(i*2+1))
		createCacheKeyMatchEntry(ctx, client, fmt.Sprintf("literal-decoy-%d", i), test.decoy, newer+int64(i*2+2))
		t.Run("literal "+test.name, func(t *testing.T) {
			match, err := service.MatchCacheEntry(
				ctx,
				[]string{"missing-" + test.name, test.prefix},
				defaultCacheEntryVersion,
				scope,
			)
			require.NoError(t, err)
			require.NotNil(t, match)
			require.Equal(t, test.match, match.Key)
		})
	}
}

func createCacheKeyMatchEntry(ctx context.Context, client *ent.Client, id string, key string, updatedAt int64) {
	location := client.StorageLocation.Create().
		SetID(id + "-location").
		SetFolderName(id + "-folder").
		SetPartCount(1).
		SaveX(ctx)
	client.CacheEntry.Create().
		SetID(id + "-entry").
		SetKey(key).
		SetVersion(defaultCacheEntryVersion).
		SetScope("refs/heads/main").
		SetRepoId("123").
		SetUpdatedAt(updatedAt).
		SetLocation(location).
		SaveX(ctx)
}

func runExternalCleanupRetention(t *testing.T, dbCfg config.DBConfig) {
	t.Helper()

	ctx := context.Background()
	client := openExternalDB(t, ctx, dbCfg)
	filesystem := testutil.NewFilesystemAdapter(t)
	now := time.Now().Truncate(time.Millisecond)
	cutoff := now.Add(-30 * 24 * time.Hour).UnixMilli()
	old := cutoff - 1
	lifecycle := storagelifecycle.NewWithOptions(client, storagelifecycle.Options{Now: func() time.Time { return now }})

	expired := createExternalRetentionLocation(ctx, client, "expired", nil, old)
	recent := createExternalRetentionLocation(ctx, client, "recent", nil, cutoff)
	shared := createExternalRetentionLocation(ctx, client, "shared", nil, old)
	client.CacheEntry.Create().
		SetID("shared-recent-entry").
		SetKey("shared-recent-key").
		SetVersion("version").
		SetScope("scope").
		SetRepoId("repo").
		SetUpdatedAt(cutoff).
		SetLocation(shared).
		SaveX(ctx)
	downloadedAt := cutoff
	downloaded := createExternalRetentionLocation(ctx, client, "downloaded", &downloadedAt, old)
	active := createExternalRetentionLocation(ctx, client, "active", nil, old)
	lease, err := lifecycle.AcquireReader(ctx, "active-entry", storagelifecycle.AcquireReaderOptions{})
	require.NoError(t, err)

	disabled := cleanup.NewService(cleanup.Options{
		DB:        client,
		Storage:   filesystem,
		Config:    config.CleanupConfig{CacheOlderThanDays: 0},
		Lifecycle: lifecycle,
		Now:       func() time.Time { return now },
	})
	deleted, err := disabled.RunCacheEntries(ctx)
	require.NoError(t, err)
	require.Zero(t, deleted)
	require.Equal(t, 6, client.CacheEntry.Query().CountX(ctx))

	service := cleanup.NewService(cleanup.Options{
		DB:        client,
		Storage:   filesystem,
		Config:    config.CleanupConfig{CacheOlderThanDays: 30},
		Lifecycle: lifecycle,
		Now:       func() time.Time { return now },
	})
	deleted, err = service.RunCacheEntries(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, deleted)
	require.NotNil(t, client.StorageLocation.GetX(ctx, expired.ID).DeletionRequestedAt)
	require.Nil(t, client.StorageLocation.GetX(ctx, recent.ID).DeletionRequestedAt)
	require.Nil(t, client.StorageLocation.GetX(ctx, shared.ID).DeletionRequestedAt)
	require.Nil(t, client.StorageLocation.GetX(ctx, downloaded.ID).DeletionRequestedAt)
	require.Nil(t, client.StorageLocation.GetX(ctx, active.ID).DeletionRequestedAt)

	client.CacheEntry.UpdateOneID("recent-entry").SetUpdatedAt(old).ExecX(ctx)
	client.CacheEntry.UpdateOneID("shared-recent-entry").SetUpdatedAt(old).ExecX(ctx)
	client.StorageLocation.UpdateOneID(downloaded.ID).SetLastDownloadedAt(old).ExecX(ctx)
	require.NoError(t, lifecycle.ReleaseReader(ctx, lease.ID))

	deleted, err = service.RunCacheEntries(ctx)
	require.NoError(t, err)
	require.Equal(t, 4, deleted)
	require.Zero(t, client.CacheEntry.Query().CountX(ctx))
}

func runExternalCapacityEviction(t *testing.T, dbCfg config.DBConfig) {
	t.Helper()

	ctx := context.Background()
	client := openExternalDB(t, ctx, dbCfg)
	filesystem := testutil.NewFilesystemAdapter(t)
	old := createExternalCapacityLocation(ctx, client, "capacity-old", 100, nil)
	downloadedAt := int64(200)
	downloaded := createExternalCapacityLocation(ctx, client, "capacity-downloaded", 50, &downloadedAt)
	recent := createExternalCapacityLocation(ctx, client, "capacity-recent", 300, nil)
	lifecycle := storagelifecycle.New(client)
	service := storagecapacity.NewService(storagecapacity.Options{
		DB:      client,
		Storage: filesystem,
		Config: config.CacheConfig{
			MaxSizeBytes:              17,
			MaxSizeBytesConfigured:    true,
			FilesystemMaxUsagePercent: 90,
		},
		Lifecycle: lifecycle,
	})

	result, err := service.Enforce(ctx)

	require.NoError(t, err)
	require.Equal(t, 1, result.ClaimedLocations)
	require.NotNil(t, client.StorageLocation.GetX(ctx, old.ID).DeletionRequestedAt)
	require.Nil(t, client.StorageLocation.GetX(ctx, downloaded.ID).DeletionRequestedAt)
	require.Nil(t, client.StorageLocation.GetX(ctx, recent.ID).DeletionRequestedAt)
}

func runExternalCapacityEvictionSkipsWhileLocked(t *testing.T, dbCfg config.DBConfig) {
	t.Helper()

	ctx := context.Background()
	client := openExternalDB(t, ctx, dbCfg)
	filesystem := testutil.NewFilesystemAdapter(t)
	old := createExternalCapacityLocation(ctx, client, "capacity-old", 100, nil)
	downloadedAt := int64(200)
	createExternalCapacityLocation(ctx, client, "capacity-downloaded", 50, &downloadedAt)
	createExternalCapacityLocation(ctx, client, "capacity-recent", 300, nil)
	service := storagecapacity.NewService(storagecapacity.Options{
		DB:      client,
		Storage: filesystem,
		Config: config.CacheConfig{
			MaxSizeBytes:              17,
			MaxSizeBytesConfigured:    true,
			FilesystemMaxUsagePercent: 90,
		},
		Lifecycle: storagelifecycle.New(client),
		Locker:    dblock.New(client),
	})
	holder, err := dblock.New(client).TryAcquire(ctx, "capacity-eviction")
	require.NoError(t, err)
	require.NotNil(t, holder)

	result, err := service.Enforce(ctx)

	require.NoError(t, err)
	require.True(t, result.Skipped)
	require.Zero(t, result.ClaimedLocations)
	require.Nil(t, client.StorageLocation.GetX(ctx, old.ID).DeletionRequestedAt)

	require.NoError(t, holder.Release())
	result, err = service.Enforce(ctx)

	require.NoError(t, err)
	require.False(t, result.Skipped)
	require.Equal(t, 1, result.ClaimedLocations)
}

func runExternalJobLockExclusion(t *testing.T, cfg config.DBConfig) {
	t.Helper()

	ctx := context.Background()
	client := openExternalDB(t, ctx, cfg)
	name := uniqueIntegrationCacheKey("job-lock")
	first := dblock.New(client)
	second := dblock.New(client)

	held, err := first.TryAcquire(ctx, name)
	require.NoError(t, err)
	require.NotNil(t, held)
	contended, err := second.TryAcquire(ctx, name)
	require.NoError(t, err)
	require.Nil(t, contended)
	require.NoError(t, held.Check(ctx))
	require.NoError(t, held.Release())

	reacquired, err := second.TryAcquire(ctx, name)
	require.NoError(t, err)
	require.NotNil(t, reacquired)
	require.NoError(t, reacquired.Release())
}

func runExternalDatabaseClockSync(t *testing.T, cfg config.DBConfig) {
	t.Helper()

	ctx := context.Background()
	client := openExternalDB(t, ctx, cfg)
	clock := dbclock.New(client, cfg.Driver, zerolog.Nop())

	require.NoError(t, clock.Sync(ctx))
	require.WithinDuration(t, time.Now(), clock.Now(), 2*time.Second)
}

func runExternalPostgresJobLockLoss(t *testing.T, cfg config.DBConfig) {
	t.Helper()

	ctx := context.Background()
	client := openExternalDB(t, ctx, cfg)
	sqlDB := openExternalSQLDB(t, ctx, cfg)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	name := uniqueIntegrationCacheKey("job-lock-loss")

	held, err := dblock.New(client).TryAcquire(ctx, name)
	require.NoError(t, err)
	require.NotNil(t, held)

	rows, err := sqlDB.QueryContext(ctx, `select pid from pg_stat_activity
		where datname = current_database()
			and pid <> pg_backend_pid()
			and state = 'idle in transaction'
			and query ilike '%job_locks%'`)
	require.NoError(t, err)
	var pids []int64
	for rows.Next() {
		var pid int64
		require.NoError(t, rows.Scan(&pid))
		pids = append(pids, pid)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Len(t, pids, 1)

	var terminated bool
	require.NoError(t, sqlDB.QueryRowContext(ctx, "select pg_terminate_backend($1)", pids[0]).Scan(&terminated))
	require.True(t, terminated)
	requireExternalRowsEventually(t, ctx, sqlDB, "terminated job lock backend did not exit",
		"select count(*) from (select 1) s where not exists (select 1 from pg_stat_activity where pid = $1)", pids[0])

	require.Error(t, held.Check(ctx))
	_ = held.Release()
	reacquired, err := dblock.New(client).TryAcquire(ctx, name)
	require.NoError(t, err)
	require.NotNil(t, reacquired)
	require.NoError(t, reacquired.Release())
}

func runExternalMySQLJobLockLoss(t *testing.T, cfg config.DBConfig) {
	t.Helper()

	ctx := context.Background()
	client := openExternalDB(t, ctx, cfg)
	sqlDB := openExternalSQLDB(t, ctx, cfg)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	// A single pooled connection makes the lock transaction run on the connection id read here.
	holderDB := openExternalSQLDB(t, ctx, cfg)
	holderDB.SetMaxOpenConns(1)
	holderClient := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.MySQL, holderDB)))
	t.Cleanup(func() { _ = holderClient.Close() })
	var holderID int64
	require.NoError(t, holderDB.QueryRowContext(ctx, "select connection_id()").Scan(&holderID))
	name := uniqueIntegrationCacheKey("job-lock-loss")

	held, err := dblock.New(holderClient).TryAcquire(ctx, name)
	require.NoError(t, err)
	require.NotNil(t, held)
	requireExternalRowsEventually(t, ctx, sqlDB, "job lock holder connection is not idle in its transaction",
		"select count(*) from information_schema.processlist where id = ? and command = 'Sleep'", holderID)

	// KILL takes no placeholder; holderID is an integer the server returned.
	_, err = sqlDB.ExecContext(ctx, fmt.Sprintf("KILL %d", holderID))
	require.NoError(t, err)
	require.Error(t, held.Check(ctx))
	_ = held.Release()

	locker := dblock.New(client)
	deadline := time.Now().Add(15 * time.Second)
	for {
		reacquired, err := locker.TryAcquire(ctx, name)
		require.NoError(t, err)
		if reacquired != nil {
			require.NoError(t, reacquired.Release())
			return
		}
		if time.Now().After(deadline) {
			require.FailNow(t, "killed job lock connection did not release the lock")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func runExternalJobClaimRun(t *testing.T, cfg config.DBConfig) {
	t.Helper()

	ctx := context.Background()
	client := openExternalDB(t, ctx, cfg)
	const interval = 24 * time.Hour
	start := time.UnixMilli(1_700_000_000_000)
	name := uniqueIntegrationCacheKey("job-claim")
	replica := dblock.New(client)
	otherReplica := dblock.New(client)

	steps := []struct {
		name   string
		locker *dblock.Locker
		now    time.Time
		want   bool
	}{
		{name: "first run creates the row", locker: replica, now: start, want: true},
		{name: "run within interval is skipped", locker: replica, now: start.Add(time.Hour), want: false},
		{name: "run at interval boundary is claimed", locker: replica, now: start.Add(interval), want: true},
		{name: "other replica in the claimed interval is skipped", locker: otherReplica, now: start.Add(interval + time.Minute), want: false},
	}
	for _, step := range steps {
		claimed, err := step.locker.ClaimRun(ctx, name, step.now, interval)
		require.NoError(t, err, step.name)
		require.Equal(t, step.want, claimed, step.name)
	}

	type claimResult struct {
		claimed bool
		err     error
	}
	const replicas = 16
	raceName := uniqueIntegrationCacheKey("job-claim-race")
	release := make(chan struct{})
	results := make(chan claimResult, replicas)
	for range replicas {
		locker := dblock.New(client)
		go func() {
			<-release
			claimed, err := locker.ClaimRun(ctx, raceName, start, interval)
			results <- claimResult{claimed: claimed, err: err}
		}()
	}
	close(release)
	claims := 0
	for range replicas {
		result := <-results
		require.NoError(t, result.err)
		if result.claimed {
			claims++
		}
	}
	require.Equal(t, 1, claims)
}

func createExternalCapacityLocation(ctx context.Context, client *ent.Client, id string, updatedAt int64, lastDownloadedAt *int64) *ent.StorageLocation {
	recency := updatedAt
	if lastDownloadedAt != nil {
		recency = *lastDownloadedAt
	}
	location := client.StorageLocation.Create().
		SetID(id + "-location").
		SetFolderName(id + "-folder").
		SetPartCount(1).
		SetSizeBytes(6).
		SetNillableLastDownloadedAt(lastDownloadedAt).
		SetRecencyAt(recency).
		SaveX(ctx)
	client.CacheEntry.Create().
		SetID(id + "-entry").
		SetKey(id + "-key").
		SetVersion("version").
		SetScope("scope").
		SetRepoId("repo").
		SetUpdatedAt(updatedAt).
		SetLocation(location).
		SaveX(ctx)
	return location
}

func createExternalRetentionLocation(ctx context.Context, client *ent.Client, id string, lastDownloadedAt *int64, updatedAt int64) *ent.StorageLocation {
	location := client.StorageLocation.Create().
		SetID(id + "-location").
		SetFolderName(id + "-folder").
		SetPartCount(1).
		SetNillableLastDownloadedAt(lastDownloadedAt).
		SaveX(ctx)
	client.CacheEntry.Create().
		SetID(id + "-entry").
		SetKey(id + "-key").
		SetVersion("version").
		SetScope("scope").
		SetRepoId("repo").
		SetUpdatedAt(updatedAt).
		SetLocation(location).
		SaveX(ctx)
	return location
}

func openExternalDB(t *testing.T, ctx context.Context, cfg config.DBConfig) *ent.Client {
	t.Helper()

	client, err := db.OpenAndMigrate(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		clearExternalDB(t, context.Background(), client)
		require.NoError(t, client.Close())
	})

	clearExternalDB(t, ctx, client)
	return client
}

func runExternalStorageSizeMigration(t *testing.T, cfg config.DBConfig) {
	t.Helper()
	ctx := context.Background()

	legacyClient, err := db.OpenAndMigrate(ctx, cfg)
	require.NoError(t, err)
	clearExternalDB(t, ctx, legacyClient)
	legacyClient.StorageLocation.Create().
		SetID("legacy-size-location").
		SetFolderName("legacy-size-folder").
		SetPartCount(1).
		SaveX(ctx)
	require.NoError(t, legacyClient.Close())

	sqlDB := openExternalSQLDB(t, ctx, cfg)
	dropSizeColumn := `alter table storage_locations drop column "sizeBytes"`
	if cfg.Driver == db.DriverMySQL {
		dropSizeColumn = "alter table storage_locations drop column `sizeBytes`"
	}
	_, err = sqlDB.ExecContext(ctx, dropSizeColumn)
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())

	client, err := db.OpenAndMigrate(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		clearExternalDB(t, context.Background(), client)
		require.NoError(t, client.Close())
	})

	legacyLocation := client.StorageLocation.GetX(ctx, "legacy-size-location")
	require.Nil(t, legacyLocation.SizeBytes)
	client.StorageLocation.UpdateOneID(legacyLocation.ID).SetSizeBytes(123).ExecX(ctx)
	updatedLocation := client.StorageLocation.GetX(ctx, legacyLocation.ID)
	require.NotNil(t, updatedLocation.SizeBytes)
	require.Equal(t, int64(123), *updatedLocation.SizeBytes)
}

func openExternalSQLDB(t *testing.T, ctx context.Context, cfg config.DBConfig) *sql.DB {
	t.Helper()

	driverName := "pgx"
	dsn := cfg.PostgresURL
	if cfg.Driver == db.DriverMySQL {
		driverName = "mysql"
		mysqlCfg := mysql.NewConfig()
		mysqlCfg.User = cfg.MySQLUser
		mysqlCfg.Passwd = cfg.MySQLPassword
		mysqlCfg.Net = "tcp"
		mysqlCfg.Addr = net.JoinHostPort(cfg.MySQLHost, cfg.MySQLPort)
		mysqlCfg.DBName = cfg.MySQLDatabase
		mysqlCfg.ParseTime = true
		dsn = mysqlCfg.FormatDSN()
	}

	sqlDB, err := sql.Open(driverName, dsn)
	require.NoError(t, err)
	require.NoError(t, sqlDB.PingContext(ctx))
	return sqlDB
}

func useCaseInsensitiveMySQLCacheKeyCollation(t *testing.T, ctx context.Context, cfg config.DBConfig) {
	t.Helper()

	alter := func(statement string) {
		sqlDB := openExternalSQLDB(t, ctx, cfg)
		_, err := sqlDB.ExecContext(ctx, statement)
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close())
	}

	alter("ALTER TABLE cache_entries MODIFY COLUMN `key` varchar(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci NOT NULL")
	t.Cleanup(func() {
		alter("ALTER TABLE cache_entries MODIFY COLUMN `key` varchar(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL")
	})
}

func clearExternalDB(t *testing.T, ctx context.Context, client *ent.Client) {
	t.Helper()

	_, err := client.Upload.Delete().Exec(ctx)
	require.NoError(t, err)
	_, err = client.CacheEntry.Delete().Exec(ctx)
	require.NoError(t, err)
	_, err = client.StorageReaderLease.Delete().Exec(ctx)
	require.NoError(t, err)
	_, err = client.StorageLocation.Delete().Exec(ctx)
	require.NoError(t, err)
}

func newExternalRouter(t *testing.T, client *ent.Client, storageAdapter storage.Adapter) http.Handler {
	t.Helper()

	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Cache.DownloadURLSigningSecret = "integration-test-secret"
	lifecycle := storagelifecycle.New(client)
	cacheService := newTestCacheService(t, cache.Options{
		DB:               client,
		Storage:          storageAdapter,
		MergeConcurrency: cfg.Cache.MergeConcurrency,
		Lifecycle:        lifecycle,
	})
	t.Cleanup(func() {
		cacheService.StopAcceptingMerges()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		require.NoError(t, cacheService.WaitForMerges(ctx))
	})

	return newRouterWithCleanup(t, cfg, httpapi.Dependencies{
		DB:        client,
		Storage:   storageAdapter,
		Cache:     cacheService,
		Lifecycle: lifecycle,
		Verifier:  newSkipVerifier(t),
	})
}

func externalPostgresConfig() (config.DBConfig, bool) {
	dsn := strings.TrimSpace(os.Getenv("E2E_POSTGRES_URL"))
	if dsn == "" {
		return config.DBConfig{}, false
	}
	return config.DBConfig{
		Driver:      db.DriverPostgres,
		PostgresURL: dsn,
	}, true
}

func externalMySQLConfig() (config.DBConfig, bool) {
	host := strings.TrimSpace(os.Getenv("E2E_MYSQL_HOST"))
	database := strings.TrimSpace(os.Getenv("E2E_MYSQL_DATABASE"))
	user := strings.TrimSpace(os.Getenv("E2E_MYSQL_USER"))
	if host == "" || database == "" || user == "" {
		return config.DBConfig{}, false
	}
	return config.DBConfig{
		Driver:        db.DriverMySQL,
		MySQLHost:     host,
		MySQLPort:     envOrDefault("E2E_MYSQL_PORT", "3306"),
		MySQLDatabase: database,
		MySQLUser:     user,
		MySQLPassword: os.Getenv("E2E_MYSQL_PASSWORD"),
	}, true
}

func externalMySQLMigrationLockName(database string) string {
	digest := sha256.Sum256([]byte(database))
	return "gacs:schema-migration:" + hex.EncodeToString(digest[:20])
}

func startExternalMigration(ctx context.Context, cfg config.DBConfig) <-chan externalMigrationResult {
	resultCh := make(chan externalMigrationResult, 1)
	go func() {
		client, err := db.OpenAndMigrate(ctx, cfg)
		resultCh <- externalMigrationResult{client: client, err: err}
	}()
	return resultCh
}

func requireMigrationStillWaiting(t *testing.T, resultCh <-chan externalMigrationResult) {
	t.Helper()
	select {
	case result := <-resultCh:
		if result.client != nil {
			require.NoError(t, result.client.Close())
		}
		require.FailNow(t, "migration returned before the database lock was released", "error: %v", result.err)
	case <-time.After(250 * time.Millisecond):
	}
}

// requireExternalRowsEventually polls on the test goroutine so query errors can fail immediately.
func requireExternalRowsEventually(t *testing.T, ctx context.Context, sqlDB *sql.DB, failure, countQuery string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var rows int64
		require.NoError(t, sqlDB.QueryRowContext(ctx, countQuery, args...).Scan(&rows))
		if rows > 0 {
			return
		}
		if time.Now().After(deadline) {
			require.FailNow(t, failure)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func requireExternalMigrationSuccess(t *testing.T, resultCh <-chan externalMigrationResult) {
	t.Helper()
	select {
	case result := <-resultCh:
		require.NoError(t, result.err)
		if result.client != nil {
			require.NoError(t, result.client.Close())
		}
	case <-time.After(15 * time.Second):
		require.FailNow(t, "migration did not finish after the database lock was released")
	}
}

func prepareExternalSizeColumnMigration(t *testing.T, ctx context.Context, cfg config.DBConfig) {
	t.Helper()
	t.Cleanup(func() {
		client, err := db.OpenAndMigrate(context.Background(), cfg)
		require.NoError(t, err)
		if client != nil {
			require.NoError(t, client.Close())
		}
	})
	client, err := db.OpenAndMigrate(ctx, cfg)
	require.NoError(t, err)
	require.NoError(t, client.Close())

	sqlDB := openExternalSQLDB(t, ctx, cfg)
	dropSizeColumn := `alter table storage_locations drop column "sizeBytes"`
	if cfg.Driver == db.DriverMySQL {
		dropSizeColumn = "alter table storage_locations drop column `sizeBytes`"
	}
	_, err = sqlDB.ExecContext(ctx, dropSizeColumn)
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
}

func requireExternalSizeColumn(t *testing.T, ctx context.Context, cfg config.DBConfig) {
	t.Helper()
	sqlDB := openExternalSQLDB(t, ctx, cfg)
	defer func() { require.NoError(t, sqlDB.Close()) }()

	query := `select exists(
		select 1 from information_schema.columns
		where table_schema = current_schema()
			and table_name = 'storage_locations'
			and column_name = 'sizeBytes'
	)`
	if cfg.Driver == db.DriverMySQL {
		query = `select exists(
			select 1 from information_schema.columns
			where table_schema = database()
				and table_name = 'storage_locations'
				and column_name = 'sizeBytes'
		)`
	}
	var exists bool
	require.NoError(t, sqlDB.QueryRowContext(ctx, query).Scan(&exists))
	require.True(t, exists)
}

func externalS3Config() (config.StorageConfig, bool) {
	endpoint := strings.TrimSpace(os.Getenv("E2E_S3_ENDPOINT_URL"))
	bucket := strings.TrimSpace(os.Getenv("E2E_S3_BUCKET"))
	if endpoint == "" || bucket == "" {
		return config.StorageConfig{}, false
	}
	return config.StorageConfig{
		Driver:           storage.DriverS3,
		S3Bucket:         bucket,
		S3Region:         envOrDefault("E2E_S3_REGION", "us-east-1"),
		S3EndpointURL:    endpoint,
		S3ForcePathStyle: true,
		S3KeyPrefix:      "gh-actions-cache-e2e",
	}, true
}

func ensureExternalS3Bucket(t *testing.T, ctx context.Context, cfg config.StorageConfig) {
	t.Helper()

	if os.Getenv("E2E_S3_CREATE_BUCKET") != "true" {
		return
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.S3Region))
	require.NoError(t, err)
	client := s3.NewFromConfig(awsCfg, func(options *s3.Options) {
		options.UsePathStyle = cfg.S3ForcePathStyle
		if cfg.S3EndpointURL != "" {
			options.BaseEndpoint = aws.String(cfg.S3EndpointURL)
		}
	})

	_, err = client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(cfg.S3Bucket),
	})
	if err != nil && !isBucketAlreadyExists(err) {
		require.NoError(t, err)
	}
}

func isBucketAlreadyExists(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.ErrorCode() {
	case "BucketAlreadyExists", "BucketAlreadyOwnedByYou":
		return true
	default:
		return false
	}
}

func uniqueIntegrationCacheKey(prefix string) string {
	return fmt.Sprintf("%s-cache-%d", prefix, time.Now().UnixNano())
}

func envOrDefault(key string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}
