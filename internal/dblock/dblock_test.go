package dblock_test

import (
	"testing"
	"time"

	"github.com/NCUnix/GitHubActionCacheServer/internal/dblock"
	"github.com/NCUnix/GitHubActionCacheServer/internal/testutil"
	"github.com/stretchr/testify/require"
)

func TestClaimRunAllowsOneStartPerInterval(t *testing.T) {
	ctx, client := testutil.NewSQLiteClient(t)
	const interval = 24 * time.Hour
	start := time.UnixMilli(1_700_000_000_000)
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
		{name: "nil locker always claims", locker: nil, now: start.Add(interval + time.Minute), want: true},
	}
	for _, step := range steps {
		claimed, err := step.locker.ClaimRun(ctx, "job", step.now, interval)
		require.NoError(t, err, step.name)
		require.Equal(t, step.want, claimed, step.name)
	}
}

func TestClaimRunClaimsExistingRowWithoutStart(t *testing.T) {
	ctx, client := testutil.NewSQLiteClient(t)
	client.JobLock.Create().SetID("job").ExecX(ctx)

	claimed, err := dblock.New(client).ClaimRun(ctx, "job", time.UnixMilli(1_700_000_000_000), time.Hour)
	require.NoError(t, err)
	require.True(t, claimed)
}
