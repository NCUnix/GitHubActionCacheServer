package cache

import (
	"fmt"
	"strings"
	"testing"

	"github.com/NCUnix/GitHubActionCacheServer/internal/auth"
	"github.com/NCUnix/GitHubActionCacheServer/internal/ent/cacheentry"
	"github.com/NCUnix/GitHubActionCacheServer/internal/ent/storagelocation"
	"github.com/NCUnix/GitHubActionCacheServer/internal/testutil"
	"github.com/stretchr/testify/require"
)

func TestLookupBatchesSQLInMatchOrder(t *testing.T) {
	ctx, client, log := testutil.NewRecordingSQLiteClient(t)
	adapter := testutil.NewFilesystemAdapter(t)
	service := newTestService(t, Options{DB: client, Storage: adapter})
	scope := auth.CacheScope{RepoID: "123", Scopes: []auth.Scope{
		{Scope: "refs/heads/base", Permission: 1},
		{Scope: "refs/heads/main", Permission: 2},
		{Scope: "refs/heads/other", Permission: 2},
	}}
	for _, entry := range []struct{ id, key, scope string }{
		{"primary", "primary", "refs/heads/main"},
		{"older-prefix", "fallback-old", "refs/heads/main"},
		{"newer-prefix", "fallback-new", "refs/heads/main"},
		{"lower-scope-exact", "fallback", "refs/heads/base"},
		{"later-key", "later", "refs/heads/main"},
		{"secondary", "missing", "refs/heads/base"},
		{"equal-permission", "missing", "refs/heads/other"},
	} {
		match := createMatchedCacheEntryInScope(ctx, client, entry.id, entry.key, entry.scope)
		location := client.StorageLocation.GetX(ctx, match.LocationId)
		require.NoError(t, adapter.UploadStream(ctx, partObjectName(location.FolderName, 0), strings.NewReader(entry.id)))
	}
	client.CacheEntry.UpdateOneID("older-prefix").SetUpdatedAt(1).ExecX(ctx)
	client.CacheEntry.UpdateOneID("newer-prefix").SetUpdatedAt(2).ExecX(ctx)

	longLookup := make([]string, cacheLookupBatchSize/2+1)
	for index := range longLookup {
		longLookup[index] = fmt.Sprintf("absent-%d", index)
	}
	longLookup = append(longLookup, "primary")
	for _, test := range []struct {
		name    string
		keys    []string
		want    string
		queries int
	}{
		{"first exact", []string{"primary", "fallback"}, "primary", 2},
		{"first scope and earlier key prefix", []string{"missing", "fallback", "later"}, "newer-prefix", 2},
		{"primary prefix beats lower scope exact", []string{"fallback"}, "newer-prefix", 2},
		{"same-permission scope order", []string{"missing"}, "equal-permission", 2},
		{"all miss", []string{"absent", "absent-two", "absent-three", "absent-four"}, "", 2},
		{"second batch", longLookup, "primary", 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			log.Reset()
			match, err := service.GetCacheEntryWithDownloadURL(ctx, test.keys, "version", scope, func(id string) string { return id })
			require.NoError(t, err)
			if test.want == "" {
				require.Nil(t, match)
			} else {
				require.NotNil(t, match)
				require.Equal(t, test.want, match.DownloadURL)
			}
			require.Equal(t, test.queries, len(log.Lines()))
		})
	}
}

func TestLookupFallsBackAfterBothDuplicateExactEntriesBecomeDangling(t *testing.T) {
	ctx, client, _ := testutil.NewRecordingSQLiteClient(t)
	adapter := testutil.NewFilesystemAdapter(t)
	service := newTestService(t, Options{DB: client, Storage: adapter})
	scope := writableScope()
	first := createMatchedCacheEntry(ctx, client, "first", "key")
	second := createMatchedCacheEntry(ctx, client, "second", "key")
	prefix := createMatchedCacheEntry(ctx, client, "prefix", "key-prefix")
	for _, locationID := range []string{first.LocationId, second.LocationId, prefix.LocationId} {
		location := client.StorageLocation.GetX(ctx, locationID)
		require.NoError(t, adapter.UploadStream(ctx, partObjectName(location.FolderName, 0), strings.NewReader("live")))
	}
	keys := []string{"absent", "key"}
	fallbackURL := func(id string) string { return id }
	match, err := service.GetCacheEntryWithDownloadURL(ctx, keys, "version", scope, fallbackURL)
	require.NoError(t, err)
	require.NotNil(t, match)
	require.Contains(t, []string{first.ID, second.ID}, match.CacheEntry.ID)

	_, err = client.ExecContext(ctx, "PRAGMA foreign_keys = OFF")
	require.NoError(t, err)
	deleted, deleteErr := client.StorageLocation.Delete().Where(storagelocation.IDIn(first.LocationId, second.LocationId)).Exec(ctx)
	_, err = client.ExecContext(ctx, "PRAGMA foreign_keys = ON")
	require.NoError(t, err)
	require.NoError(t, deleteErr)
	require.Equal(t, 2, deleted)

	match, err = service.GetCacheEntryWithDownloadURL(ctx, keys, "version", scope, fallbackURL)
	require.NoError(t, err)
	require.NotNil(t, match)
	require.Equal(t, prefix.ID, match.CacheEntry.ID)
	require.False(t, client.CacheEntry.Query().Where(cacheentry.IDIn(first.ID, second.ID)).ExistX(ctx))
}
