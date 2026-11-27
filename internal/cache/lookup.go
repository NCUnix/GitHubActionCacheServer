package cache

import (
	"context"
	"fmt"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	"github.com/NCUnix/GitHubActionCacheServer/internal/auth"
	"github.com/NCUnix/GitHubActionCacheServer/internal/cachekey"
	"github.com/NCUnix/GitHubActionCacheServer/internal/ent"
	"github.com/NCUnix/GitHubActionCacheServer/internal/ent/cacheentry"
	"github.com/NCUnix/GitHubActionCacheServer/internal/ent/storagelocation"
)

const cacheLookupBatchSize = 32

// cacheLookupSnapshot holds read-only lookup data. Joined results load a subset of location fields and have no Ent driver.
type cacheLookupSnapshot struct {
	entry    *ent.CacheEntry
	location *ent.StorageLocation
}

type cacheLookupBranch struct {
	key    string
	scope  string
	prefix bool
}

func (branch cacheLookupBranch) apply(selector *sql.Selector, version, repoID string) {
	cacheentry.Version(version)(selector)
	cacheentry.RepoId(repoID)(selector)
	cacheentry.Scope(branch.scope)(selector)
	if branch.prefix {
		cachekey.Prefix(branch.key)(selector)
		selector.OrderBy(sql.Desc(selector.C(cacheentry.FieldUpdatedAt)))
	} else {
		cachekey.Exact(branch.key)(selector)
	}
}

func (s *Service) lookupCacheEntry(ctx context.Context, keys []string, version string, scope auth.CacheScope) (*cacheLookupSnapshot, error) {
	scopes := ReadScopesByPermission(scope)
	if len(keys) == 0 || len(scopes) == 0 {
		return nil, nil
	}

	exact := cacheLookupBranch{key: keys[0], scope: scopes[0]}
	match, err := s.lookupFirstExact(ctx, exact, version, scope.RepoID)
	if err != nil || match != nil {
		return match, err
	}

	var branches [cacheLookupBatchSize]cacheLookupBranch
	batch := branches[:0]
	scanBatch := func() (*cacheLookupSnapshot, error) {
		return s.scanCacheLookup(ctx, func(dialect *sql.DialectBuilder, selector *sql.Selector) {
			entryID := selector.C(cacheentry.FieldID)
			selector.Where(sql.P(func(builder *sql.Builder) {
				builder.Ident(entryID).WriteString(" = COALESCE")
				builder.Wrap(func(builder *sql.Builder) {
					for index, branch := range batch {
						if index > 0 {
							builder.Comma()
						}
						entries := dialect.Table(cacheentry.Table)
						candidate := dialect.Select(entries.C(cacheentry.FieldID)).From(entries).Limit(1)
						branch.apply(candidate, version, scope.RepoID)
						builder.Wrap(func(builder *sql.Builder) { builder.Join(candidate) })
					}
					builder.WriteString(", NULL")
				})
			}))
		})
	}
	for scopeIndex, cacheScope := range scopes {
		for keyIndex, key := range keys {
			for _, prefix := range [...]bool{false, true} {
				if scopeIndex == 0 && keyIndex == 0 && !prefix {
					continue
				}
				batch = append(batch, cacheLookupBranch{key: key, scope: cacheScope, prefix: prefix})
				if len(batch) == cacheLookupBatchSize {
					match, err := scanBatch()
					if err != nil || match != nil {
						return match, err
					}
					batch = batch[:0]
				}
			}
		}
	}
	if len(batch) == 0 {
		return nil, nil
	}
	return scanBatch()
}

func (s *Service) lookupFirstExact(ctx context.Context, branch cacheLookupBranch, version, repoID string) (*cacheLookupSnapshot, error) {
	if s.db.Dialect() != dialect.SQLite {
		return s.scanCacheLookup(ctx, func(_ *sql.DialectBuilder, selector *sql.Selector) {
			branch.apply(selector, version, repoID)
		})
	}

	entry, err := s.db.CacheEntry.Query().
		Where(cacheentry.Version(version), cacheentry.RepoId(repoID), cacheentry.Scope(branch.scope), cachekey.Exact(branch.key)).
		First(ctx)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query cache entry: %w", err)
	}
	location, err := s.db.StorageLocation.Get(ctx, entry.LocationId)
	if err != nil && !ent.IsNotFound(err) {
		return nil, fmt.Errorf("query matched storage location: %w", err)
	}
	return &cacheLookupSnapshot{entry: entry, location: location}, nil
}

func (s *Service) scanCacheLookup(ctx context.Context, filter func(*sql.DialectBuilder, *sql.Selector)) (*cacheLookupSnapshot, error) {
	rows, err := s.db.QuerySelector(ctx, func(dialect *sql.DialectBuilder) *sql.Selector {
		entries := dialect.Table(cacheentry.Table)
		locations := dialect.Table(storagelocation.Table)
		selector := dialect.Select(entries.Columns(
			cacheentry.FieldID, cacheentry.FieldKey, cacheentry.FieldVersion, cacheentry.FieldScope,
			cacheentry.FieldRepoId, cacheentry.FieldUpdatedAt, cacheentry.FieldLocationId,
		)...).From(entries).Limit(1)
		filter(dialect, selector)
		return selector.LeftJoin(locations).
			On(entries.C(cacheentry.FieldLocationId), locations.C(storagelocation.FieldID)).
			AppendSelect(locations.Columns(
				storagelocation.FieldID, storagelocation.FieldFolderName, storagelocation.FieldPartCount,
				storagelocation.FieldLeaseVersion, storagelocation.FieldDeletionRequestedAt,
				storagelocation.FieldMergeStartedAt, storagelocation.FieldMergeLeaseToken, storagelocation.FieldMergeLeaseExpiresAt,
				storagelocation.FieldMergedAt, storagelocation.FieldMaterializationUnsupportedAt,
				storagelocation.FieldPartsDeletedAt, storagelocation.FieldManifestAnchor,
			)...)
	})
	if err != nil {
		return nil, fmt.Errorf("query cache entry and storage location: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	entry, location := &ent.CacheEntry{}, &ent.StorageLocation{}
	var locationID, folder *string
	var partCount *int
	var leaseVersion *int64
	if err := rows.Scan(
		&entry.ID, &entry.Key, &entry.Version, &entry.Scope, &entry.RepoId, &entry.UpdatedAt, &entry.LocationId,
		&locationID, &folder, &partCount, &leaseVersion, &location.DeletionRequestedAt,
		&location.MergeStartedAt, &location.MergeLeaseToken, &location.MergeLeaseExpiresAt,
		&location.MergedAt, &location.MaterializationUnsupportedAt, &location.PartsDeletedAt, &location.ManifestAnchor,
	); err != nil {
		return nil, fmt.Errorf("scan cache entry and storage location: %w", err)
	}
	if locationID == nil {
		return &cacheLookupSnapshot{entry: entry}, nil
	}
	if folder == nil || partCount == nil || leaseVersion == nil {
		return nil, fmt.Errorf("incomplete storage location %q", *locationID)
	}
	location.ID = *locationID
	location.FolderName = *folder
	location.PartCount = *partCount
	location.LeaseVersion = *leaseVersion
	return &cacheLookupSnapshot{entry: entry, location: location}, nil
}
