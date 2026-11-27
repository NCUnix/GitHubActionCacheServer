package dbclock

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/NCUnix/GitHubActionCacheServer/internal/db"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestSyncUsesRoundTripMidpoint(t *testing.T) {
	t0 := time.UnixMilli(1_700_000_000_000)
	readings := []time.Time{t0, t0.Add(20 * time.Millisecond)}
	clock := &Clock{
		localNow: func() time.Time {
			next := readings[0]
			readings = readings[1:]
			return next
		},
		query: func(context.Context) (int64, error) {
			return t0.Add(10*time.Millisecond + 5*time.Second).UnixMilli(), nil
		},
	}

	require.NoError(t, clock.Sync(context.Background()))
	require.Equal(t, 5*time.Second, clock.Offset())

	later := t0.Add(time.Hour)
	clock.localNow = func() time.Time { return later }
	require.Equal(t, later.Add(5*time.Second), clock.Now())
}

func TestSyncKeepsOffsetOnQueryError(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	queryErr := errors.New("connection refused")
	clock := &Clock{
		localNow: func() time.Time { return now },
		query:    func(context.Context) (int64, error) { return 0, queryErr },
	}
	clock.offset.Store(int64(3 * time.Second))

	require.ErrorIs(t, clock.Sync(context.Background()), queryErr)
	require.Equal(t, 3*time.Second, clock.Offset())
	require.Equal(t, now.Add(3*time.Second), clock.Now())
}

func TestSQLiteSyncIsNoop(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	clock := New(nil, db.DriverSQLite, zerolog.Nop())
	require.Nil(t, clock.query)
	clock.localNow = func() time.Time { return now }

	require.NoError(t, clock.Sync(context.Background()))
	require.Equal(t, now, clock.Now())
}
