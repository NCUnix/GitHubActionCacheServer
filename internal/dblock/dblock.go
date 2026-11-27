package dblock

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/NCUnix/GitHubActionCacheServer/internal/ent"
	"github.com/NCUnix/GitHubActionCacheServer/internal/ent/joblock"
)

// Locker grants cross-replica mutual exclusion with a row lock held by an open
// transaction. A nil *Locker disables locking (single-instance SQLite).
type Locker struct {
	db *ent.Client
}

func New(db *ent.Client) *Locker {
	return &Locker{db: db}
}

type Lock struct {
	tx   *ent.Tx
	name string
}

// TryAcquire returns a nil Lock and nil error when another replica holds name.
func (l *Locker) TryAcquire(ctx context.Context, name string) (*Lock, error) {
	if l == nil {
		return &Lock{}, nil
	}
	for attempt := 0; ; attempt++ {
		lock, exists, err := l.tryLockRow(ctx, name)
		if err != nil || lock != nil || exists {
			return lock, err
		}
		if attempt > 0 {
			return nil, fmt.Errorf("job lock %q row is missing after creation", name)
		}
		// The row is created lazily and only when absent: on MySQL a duplicate
		// insert waits for the holder's row lock instead of failing fast.
		if err := l.db.JobLock.Create().SetID(name).Exec(ctx); err != nil && !ent.IsConstraintError(err) {
			return nil, fmt.Errorf("create job lock %q: %w", name, err)
		}
	}
}

// tryLockRow reports whether the lock row exists when it could not be locked.
func (l *Locker) tryLockRow(ctx context.Context, name string) (*Lock, bool, error) {
	tx, err := l.db.Tx(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin job lock %q transaction: %w", name, err)
	}
	ids, err := tx.JobLock.Query().
		Where(joblock.ID(name)).
		ForUpdate(entsql.WithLockAction(entsql.SkipLocked)).
		IDs(ctx)
	if err == nil && len(ids) == 1 {
		return &Lock{tx: tx, name: name}, true, nil
	}
	if rollbackErr := tx.Rollback(); rollbackErr != nil {
		err = errors.Join(err, fmt.Errorf("rollback: %w", rollbackErr))
	}
	if err != nil {
		return nil, false, fmt.Errorf("lock job %q: %w", name, err)
	}
	exists, err := l.db.JobLock.Query().Where(joblock.ID(name)).Exist(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("check job lock %q: %w", name, err)
	}
	return nil, exists, nil
}

// ClaimRun reports whether this replica should start name's run: it records
// now as the start unless a run started within interval. A nil Locker always claims.
func (l *Locker) ClaimRun(ctx context.Context, name string, now time.Time, interval time.Duration) (bool, error) {
	if l == nil {
		return true, nil
	}
	affected, err := l.db.JobLock.Update().
		Where(
			joblock.ID(name),
			joblock.Or(joblock.LastStartedAtIsNil(), joblock.LastStartedAtLTE(now.Add(-interval).UnixMilli())),
		).
		SetLastStartedAt(now.UnixMilli()).
		Save(ctx)
	if err != nil {
		return false, fmt.Errorf("claim job %q run: %w", name, err)
	}
	if affected == 1 {
		return true, nil
	}
	// Check before inserting so routine skips do not log a unique violation on the server.
	exists, err := l.db.JobLock.Query().Where(joblock.ID(name)).Exist(ctx)
	if err != nil {
		return false, fmt.Errorf("check job %q run: %w", name, err)
	}
	if exists {
		return false, nil
	}
	err = l.db.JobLock.Create().SetID(name).SetLastStartedAt(now.UnixMilli()).Exec(ctx)
	if err == nil {
		return true, nil
	}
	if ent.IsConstraintError(err) {
		return false, nil
	}
	return false, fmt.Errorf("create job %q run: %w", name, err)
}

// Check returns an error when the transaction holding the lock is no longer usable.
func (k *Lock) Check(ctx context.Context) error {
	if k.tx == nil {
		return nil
	}
	exists, err := k.tx.JobLock.Query().Where(joblock.ID(k.name)).Exist(ctx)
	if err != nil {
		return fmt.Errorf("job lock %q lost: %w", k.name, err)
	}
	if !exists {
		return fmt.Errorf("job lock %q lost: row is missing", k.name)
	}
	return nil
}

func (k *Lock) Release() error {
	if k.tx == nil {
		return nil
	}
	// database/sql already rolled back the transaction if its context ended.
	if err := k.tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("release job lock %q: %w", k.name, err)
	}
	return nil
}
