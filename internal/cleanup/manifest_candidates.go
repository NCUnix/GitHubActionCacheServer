package cleanup

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/NCUnix/GitHubActionCacheServer/internal/blockmanifest"
	"github.com/NCUnix/GitHubActionCacheServer/internal/ent"
	"github.com/NCUnix/GitHubActionCacheServer/internal/ent/storagemanifest"
	"github.com/NCUnix/GitHubActionCacheServer/internal/storage"
	"github.com/google/uuid"
)

func (s *Service) RunManifestCandidates(ctx context.Context) (int, error) {
	pruner, ok := s.storage.(storage.FolderPruner)
	if !ok {
		return 0, nil
	}
	now := s.now()
	if s.locker != nil {
		claimed, err := s.locker.ClaimRun(ctx, "cleanup:manifest-candidates", now, storageDeletionsInterval)
		if err != nil || !claimed {
			return 0, err
		}
	}
	cutoff := now.Add(-s.orphanedStorageGracePeriod())
	deleted := 0
	cursor := 0
	failures := &orphanErrorCollector{}
	for {
		ids, err := s.db.StorageManifest.Query().Where(
			storagemanifest.IDGT(cursor),
			storagemanifest.CandidateGCAtLTE(cutoff.UnixMilli()),
			storagemanifest.UploadIdIsNil(),
			storagemanifest.LocationIdNotNil(),
		).Order(storagemanifest.ByID()).Limit(itemsPerPage).IDs(ctx)
		if err != nil {
			failures.add(fmt.Errorf("query due manifest candidate cleanup: %w", err))
			return deleted, failures.err()
		}
		for _, id := range ids {
			cursor = id
			count, err := s.pruneManifestCandidates(ctx, pruner, id, cutoff)
			deleted += count
			if err != nil {
				failures.add(fmt.Errorf("prune manifest %d candidates: %w", id, err))
			}
		}
		if len(ids) < itemsPerPage {
			return deleted, failures.err()
		}
	}
}

func (s *Service) pruneManifestCandidates(ctx context.Context, pruner storage.FolderPruner, id int, cutoff time.Time) (int, error) {
	manifest, err := s.db.StorageManifest.Query().Where(storagemanifest.ID(id)).WithLocation().Only(ctx)
	if ent.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if manifest.CandidateGCAt == nil || *manifest.CandidateGCAt > cutoff.UnixMilli() || manifest.UploadId != nil || manifest.Edges.Location == nil {
		return 0, nil
	}
	location := manifest.Edges.Location
	parts, _, err := blockmanifest.ForRecord(manifest, location)
	if err != nil {
		return 0, err
	}
	keep := make(map[uuid.UUID]struct{}, len(parts))
	for _, part := range parts {
		keep[part.Candidate] = struct{}{}
	}
	young := false
	deleted, more, err := pruner.PruneFolderObjects(ctx, blockmanifest.Folder(location.FolderName), func(object storage.ObjectMetadata) bool {
		name, candidateObject := strings.CutSuffix(object.Name, "/data")
		candidate, err := uuid.Parse(name)
		if !candidateObject || err != nil || candidate == uuid.Nil || candidate.String() != name {
			return false
		}
		if _, published := keep[candidate]; published {
			return false
		}
		if object.ModifiedAt.IsZero() || object.ModifiedAt.After(cutoff) {
			young = true
			return false
		}
		return true
	})
	if err != nil || more || young {
		return deleted, err
	}
	_, err = s.db.StorageManifest.Update().Where(storagemanifest.ID(id), storagemanifest.CandidateGCAtEQ(*manifest.CandidateGCAt)).
		ClearCandidateGCAt().Save(ctx)
	return deleted, err
}
