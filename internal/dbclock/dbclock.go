package dbclock

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/NCUnix/GitHubActionCacheServer/internal/db"
	"github.com/NCUnix/GitHubActionCacheServer/internal/ent"
	"github.com/rs/zerolog"
)

const refreshInterval = time.Minute

// Clock reports local time corrected by the measured offset to the database
// clock, so replicas agree on lease and heartbeat timestamps.
type Clock struct {
	offset   atomic.Int64                         // nanoseconds
	query    func(context.Context) (int64, error) // database Unix milliseconds; nil for SQLite
	localNow func() time.Time
	logger   zerolog.Logger
}

func New(client *ent.Client, driver string, logger zerolog.Logger) *Clock {
	clock := &Clock{localNow: time.Now, logger: logger}
	switch driver {
	case db.DriverPostgres:
		clock.query = queryMillis(client, "select (extract(epoch from clock_timestamp()) * 1000)::bigint")
	case db.DriverMySQL:
		// UTC_TIMESTAMP ignores the session time_zone, so time zone settings and DST cannot shift the epoch.
		clock.query = queryMillis(client, "select timestampdiff(microsecond, '1970-01-01 00:00:00', utc_timestamp(6)) div 1000")
	}
	return clock
}

func queryMillis(client *ent.Client, query string) func(context.Context) (int64, error) {
	return func(ctx context.Context) (int64, error) {
		rows, err := client.QueryContext(ctx, query)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		if !rows.Next() {
			if err := rows.Err(); err != nil {
				return 0, err
			}
			return 0, sql.ErrNoRows
		}
		var millis int64
		if err := rows.Scan(&millis); err != nil {
			return 0, err
		}
		return millis, rows.Err()
	}
}

func (c *Clock) Now() time.Time {
	return c.localNow().Add(c.Offset())
}

func (c *Clock) Offset() time.Duration {
	return time.Duration(c.offset.Load())
}

func (c *Clock) Sync(ctx context.Context) error {
	if c.query == nil {
		return nil
	}
	t0 := c.localNow()
	dbMillis, err := c.query(ctx)
	if err != nil {
		return fmt.Errorf("query database clock: %w", err)
	}
	t1 := c.localNow()
	c.offset.Store(int64(time.UnixMilli(dbMillis).Sub(t0.Add(t1.Sub(t0) / 2))))
	return nil
}

func (c *Clock) Run(ctx context.Context) {
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.Sync(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				c.logger.Warn().Err(err).Dur("offset", c.Offset()).Msg("database clock sync failed; keeping previous offset")
			}
		}
	}
}
