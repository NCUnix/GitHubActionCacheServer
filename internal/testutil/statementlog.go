package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/NCUnix/GitHubActionCacheServer/internal/config"
	"github.com/NCUnix/GitHubActionCacheServer/internal/db"
	"github.com/NCUnix/GitHubActionCacheServer/internal/ent"
	"github.com/stretchr/testify/require"
)

// StatementLog records the statements ent's debug driver reports, in order.
type StatementLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *StatementLog) record(_ context.Context, values ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprint(values...))
}

func (l *StatementLog) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = nil
}

func (l *StatementLog) Lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// NewRecordingSQLiteClient opens a migrated SQLite database through ent's
// debug driver so tests can assert statement order.
func NewRecordingSQLiteClient(t testing.TB) (context.Context, *ent.Client, *StatementLog) {
	t.Helper()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	migrated, err := db.OpenAndMigrate(ctx, config.DBConfig{Driver: db.DriverSQLite, SQLitePath: path})
	require.NoError(t, err)
	require.NoError(t, migrated.Close())

	sqlDB, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	_, err = sqlDB.ExecContext(ctx, "PRAGMA foreign_keys = ON")
	if err != nil {
		_ = sqlDB.Close()
	}
	require.NoError(t, err)

	log := &StatementLog{}
	client := ent.NewClient(ent.Driver(dialect.DebugWithContext(entsql.OpenDB(dialect.SQLite, sqlDB), log.record)))
	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})
	return ctx, client, log
}

// RequireLocationLockedBeforeCacheEntryWrites asserts that locationID's row
// lock (an UPDATE of storage_locations) precedes the first cache_entries write.
// Lines have ent's debug format, e.g.
// "Tx(<id>).Exec: query=UPDATE `storage_locations` SET ... args=[1 <locationID>]".
func RequireLocationLockedBeforeCacheEntryWrites(t testing.TB, lines []string, locationID string) {
	t.Helper()

	lockIndex := LocationLockIndex(lines, locationID)
	writeIndex := -1
	for index, line := range lines {
		if isCacheEntryWrite(line) {
			writeIndex = index
			break
		}
	}
	joined := strings.Join(lines, "\n")
	require.GreaterOrEqual(t, lockIndex, 0, "no lock of storage location %q in:\n%s", locationID, joined)
	require.GreaterOrEqual(t, writeIndex, 0, "no cache_entries write in:\n%s", joined)
	require.Less(t, lockIndex, writeIndex, "storage location %q locked after cache_entries write:\n%s", locationID, joined)
}

// LocationLockIndex returns the index of the first UPDATE of storage_locations
// bound to locationID, or -1.
func LocationLockIndex(lines []string, locationID string) int {
	for index, line := range lines {
		query, args, ok := splitStatement(line)
		if ok && strings.HasPrefix(query, "UPDATE `storage_locations`") && slices.Contains(strings.Fields(strings.Trim(args, "[]")), locationID) {
			return index
		}
	}
	return -1
}

func isCacheEntryWrite(line string) bool {
	query, _, ok := splitStatement(line)
	return ok && (strings.HasPrefix(query, "UPDATE `cache_entries`") || strings.HasPrefix(query, "DELETE FROM `cache_entries`"))
}

func splitStatement(line string) (query, args string, ok bool) {
	_, rest, found := strings.Cut(line, "query=")
	if !found {
		return "", "", false
	}
	query, args, found = strings.Cut(rest, " args=")
	return query, args, found
}
