package cache

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/NCUnix/GitHubActionCacheServer/internal/blockmanifest"
	"github.com/NCUnix/GitHubActionCacheServer/internal/ent"
	"github.com/NCUnix/GitHubActionCacheServer/internal/ent/storagereaderlease"
	"github.com/NCUnix/GitHubActionCacheServer/internal/storage"
	"github.com/NCUnix/GitHubActionCacheServer/internal/storagelifecycle"
	"github.com/google/uuid"
)

func (s *Service) DownloadRanged(ctx context.Context, cacheEntryID string, ranges []ByteRange) (*DownloadStream, error) {
	if len(ranges) == 0 {
		return nil, fmt.Errorf("download ranged cache entry: no byte ranges")
	}
	return s.download(ctx, cacheEntryID, ranges)
}

func (s *Service) download(ctx context.Context, cacheEntryID string, ranges []ByteRange) (*DownloadStream, error) {
	lease, err := s.lifecycle.AcquireReader(ctx, cacheEntryID, storagelifecycle.AcquireReaderOptions{})
	if err != nil {
		if errors.Is(err, storagelifecycle.ErrLocationUnavailable) {
			return nil, ErrCacheNotFound
		}
		return nil, &DownloadError{Err: err, CacheEntryID: cacheEntryID}
	}
	location := lease.Location
	representation := string(storagereaderlease.ScopeParts)
	if lease.Scope == storagereaderlease.ScopeStorage {
		representation = string(storagereaderlease.ScopeStorage)
	}

	var (
		stream      io.ReadCloser
		resolved    DownloadRange
		totalLength = contentLength(location.SizeBytes)
	)
	if len(ranges) == 0 {
		if lease.Scope == storagereaderlease.ScopeStorage {
			stream, err = s.openMerged(ctx, location)
		} else {
			s.tryStartMaterialization(location, cacheEntryID)
			stream, err = s.openParts(ctx, lease.ID, location)
		}
	} else if lease.Scope == storagereaderlease.ScopeStorage {
		stream, totalLength, resolved, err = s.openRangedMerged(ctx, location, ranges)
	} else {
		s.tryStartMaterialization(location, cacheEntryID)
		stream, totalLength, resolved, err = s.openRangedParts(ctx, lease.ID, location, ranges)
	}
	if err != nil {
		s.releaseReaderLease(lease.ID)
		return nil, &DownloadError{
			Err:               err,
			CacheEntryID:      cacheEntryID,
			StorageLocationID: location.ID,
			Representation:    representation,
		}
	}

	responseLength := totalLength
	var selected *DownloadRange
	if len(ranges) != 0 {
		responseLength = resolved.Count
		selected = &resolved
	}
	s.touchStorageLocationIfStale(ctx, location)
	return &DownloadStream{
		ReadCloser:        newLeasedReadCloser(stream, s.lifecycle, lease, s.enqueueReaderLeaseRelease),
		CacheEntryID:      cacheEntryID,
		StorageLocationID: location.ID,
		Representation:    representation,
		ContentLength:     responseLength,
		TotalLength:       totalLength,
		Range:             selected,
	}, nil
}

func (s *Service) openRangedMerged(ctx context.Context, location *ent.StorageLocation, ranges []ByteRange) (io.ReadCloser, int64, DownloadRange, error) {
	total, err := s.objectSize(ctx, mergedObjectName(location.FolderName), location.SizeBytes)
	if err != nil {
		return nil, 0, DownloadRange{}, err
	}
	selected, err := resolveByteRanges(ranges, total)
	if err != nil {
		return nil, total, DownloadRange{}, err
	}
	stream, err := s.storage.CreateRangedDownloadStream(ctx, mergedObjectName(location.FolderName), selected.Offset, selected.Count)
	if errors.Is(err, storage.ErrObjectNotFound) {
		err = ErrCacheNotFound
	}
	return stream, total, selected, err
}

func (s *Service) openRangedParts(ctx context.Context, leaseID string, location *ent.StorageLocation, ranges []ByteRange) (io.ReadCloser, int64, DownloadRange, error) {
	if location.PartCount < 0 {
		return nil, 0, DownloadRange{}, fmt.Errorf("invalid finalized part count %d", location.PartCount)
	}
	if location.PartCount == 0 {
		selected, err := resolveByteRanges(ranges, 0)
		return nil, 0, selected, err
	}
	if location.PartCount == 1 {
		objectName := anchorObjectName(location)
		total, err := s.objectSize(ctx, objectName, location.SizeBytes)
		if err != nil {
			return nil, 0, DownloadRange{}, err
		}
		selected, err := resolveByteRanges(ranges, total)
		if err != nil {
			return nil, total, DownloadRange{}, err
		}
		stream, err := s.storage.CreateRangedDownloadStream(ctx, objectName, selected.Offset, selected.Count)
		if errors.Is(err, storage.ErrObjectNotFound) {
			err = ErrCacheNotFound
		}
		return stream, total, selected, err
	}

	knownTotal := contentLength(location.SizeBytes)
	var selected DownloadRange
	if knownTotal >= 0 {
		resolved, err := resolveByteRanges(ranges, knownTotal)
		if err != nil {
			return nil, knownTotal, DownloadRange{}, err
		}
		selected = resolved
	}

	index, delayed, err := s.cachedPartIndex(ctx, location)
	if err != nil {
		if errors.Is(err, storage.ErrIndexedObjectMissing) {
			return nil, 0, DownloadRange{}, ErrCacheNotFound
		}
		return nil, 0, DownloadRange{}, fmt.Errorf("inspect ranged cache parts: %w", err)
	}
	total := index.total
	if knownTotal >= 0 && total != knownTotal {
		return nil, total, DownloadRange{}, fmt.Errorf("ranged cache part sizes total %d does not match recorded size %d", total, knownTotal)
	}
	if knownTotal < 0 {
		selected, err = resolveByteRanges(ranges, total)
		if err != nil {
			return nil, total, DownloadRange{}, err
		}
	}

	if delayed {
		if err := s.revalidatePartReader(ctx, leaseID); err != nil {
			return nil, total, DownloadRange{}, err
		}
	}
	return newRangedPartsReadCloser(ctx, s.storage, location.FolderName, index, selected), total, selected, nil
}

func (s *Service) revalidatePartReader(ctx context.Context, leaseID string) error {
	renewCtx, cancel := context.WithTimeout(ctx, mergeCleanupTimeout)
	defer cancel()
	err := s.lifecycle.RenewReader(renewCtx, leaseID)
	if errors.Is(err, storagelifecycle.ErrReaderLeaseLost) {
		return ErrCacheNotFound
	}
	if err != nil {
		return fmt.Errorf("revalidate cache part reader lease: %w", err)
	}
	return nil
}

func (s *Service) cachedPartIndex(ctx context.Context, location *ent.StorageLocation) (*partIndex, bool, error) {
	key := partIndexKey{
		locationID: location.ID,
		folderName: location.FolderName,
		partCount:  location.PartCount,
	}
	return s.partIndexes.getOrLoad(ctx, key, func(loadCtx context.Context) (*partIndex, error) {
		discoveryCtx, cancel := context.WithTimeout(loadCtx, s.partIndexTimeout)
		defer cancel()
		if location.ManifestAnchor != nil {
			parts, total, err := blockmanifest.ForLocation(discoveryCtx, s.db, location)
			if err != nil {
				return nil, err
			}
			index := &partIndex{ends: make([]int64, len(parts)), candidates: make([]uuid.UUID, len(parts)), total: total}
			var end int64
			for position, part := range parts {
				end += part.Size
				index.ends[position] = end
				index.candidates[position] = part.Candidate
			}
			return index, nil
		}
		sizes, err := s.storage.InspectIndexedFolderSizes(discoveryCtx, partsFolderName(location.FolderName), location.PartCount)
		if err != nil {
			return nil, err
		}
		return newPartIndex(sizes)
	})
}

func (s *Service) objectSize(ctx context.Context, objectName string, recorded *int64) (int64, error) {
	if recorded != nil && *recorded >= 0 {
		return *recorded, nil
	}
	object, err := s.storage.InspectObject(ctx, objectName)
	if errors.Is(err, storage.ErrObjectNotFound) {
		return 0, ErrCacheNotFound
	}
	if err != nil {
		return 0, err
	}
	return object.SizeBytes, nil
}

func (s *Service) DownloadMetadata(ctx context.Context, cacheEntryID string) (*DownloadMetadata, error) {
	snapshot, err := s.lifecycle.InspectReader(ctx, cacheEntryID, storagelifecycle.AcquireReaderOptions{})
	if err != nil {
		if errors.Is(err, storagelifecycle.ErrLocationUnavailable) {
			return nil, ErrCacheNotFound
		}
		return nil, &DownloadError{Err: fmt.Errorf("query download metadata: %w", err), CacheEntryID: cacheEntryID}
	}
	location := snapshot.Location

	representation := string(storagereaderlease.ScopeParts)
	objectName := ""
	if snapshot.Scope == storagereaderlease.ScopeStorage {
		representation = string(storagereaderlease.ScopeStorage)
		objectName = mergedObjectName(location.FolderName)
	} else if location.PartCount > 0 {
		objectName = anchorObjectName(location)
	} else if location.PartCount < 0 {
		return nil, &DownloadError{Err: fmt.Errorf("invalid finalized part count %d", location.PartCount), CacheEntryID: cacheEntryID, StorageLocationID: location.ID, Representation: representation}
	}

	total := contentLength(location.SizeBytes)
	if snapshot.Scope == storagereaderlease.ScopeParts && location.PartCount == 0 {
		total = 0
	}
	if total < 0 {
		switch {
		case snapshot.Scope == storagereaderlease.ScopeStorage || location.PartCount == 1:
			total, err = s.objectSize(ctx, objectName, nil)
		case location.PartCount == 0:
			total = 0
		default:
			var index *partIndex
			index, _, err = s.cachedPartIndex(ctx, location)
			if err == nil {
				total = index.total
			}
		}
	} else if objectName != "" {
		var exists bool
		exists, err = s.storage.ObjectExists(ctx, objectName)
		if err == nil && !exists {
			err = ErrCacheNotFound
		}
	}
	if errors.Is(err, storage.ErrIndexedObjectMissing) || errors.Is(err, storage.ErrObjectNotFound) {
		err = ErrCacheNotFound
	}
	if err != nil {
		return nil, &DownloadError{Err: err, CacheEntryID: cacheEntryID, StorageLocationID: location.ID, Representation: representation}
	}
	return &DownloadMetadata{
		CacheEntryID:      cacheEntryID,
		StorageLocationID: location.ID,
		Representation:    representation,
		ContentLength:     total,
	}, nil
}
