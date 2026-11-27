package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"

	"entgo.io/ent/dialect/sql"
	"github.com/NCUnix/GitHubActionCacheServer/internal/blockmanifest"
	"github.com/NCUnix/GitHubActionCacheServer/internal/ent"
	"github.com/NCUnix/GitHubActionCacheServer/internal/ent/storagelocation"
	"github.com/NCUnix/GitHubActionCacheServer/internal/ent/storagemanifest"
	"github.com/NCUnix/GitHubActionCacheServer/internal/ent/upload"
	"github.com/NCUnix/GitHubActionCacheServer/internal/ent/uploadblock"
	"github.com/NCUnix/GitHubActionCacheServer/internal/storage"
	"github.com/google/uuid"
)

type uploadByteCounter struct {
	reader io.Reader
	bytes  int64
}

func (counter *uploadByteCounter) Read(buffer []byte) (int, error) {
	count, err := counter.reader.Read(buffer)
	counter.bytes += int64(count)
	return count, err
}

func hashBlockID(blockID string) string {
	hash := sha256.Sum256([]byte(blockID))
	return hex.EncodeToString(hash[:])
}

func (s *Service) uploadManifestBlock(ctx context.Context, session *ent.Upload, blockID string, stream io.Reader) (resultErr error) {
	candidate := uuid.New()
	counter := &uploadByteCounter{reader: stream}
	size := int64(-1)
	if sized, ok := stream.(storage.SizedUpload); ok {
		size = sized.Size
	}
	attempted := false
	defer func() {
		if attempted && resultErr != nil {
			s.rescheduleCandidateGC(ctx, session.FolderName)
		}
	}()
	if err := s.withUploadActivity(ctx, session.ID, func(activityCtx context.Context) error {
		attempted = true
		return s.storage.UploadStream(activityCtx, blockmanifest.Object(session.FolderName, candidate.String()), storage.SizedUpload{Reader: counter, Size: size})
	}); err != nil {
		return err
	}
	if size >= 0 && size != counter.bytes {
		return fmt.Errorf("uploaded cache block size %d differs from declared size %d", counter.bytes, size)
	}

	receiptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), uploadActivityRenewalTimeout)
	defer cancel()
	return s.withManifestUploadLock(receiptCtx, session.ID, func(tx *ent.Tx) error {
		if blockID == "" {
			return saveUploadManifest(receiptCtx, tx, session.ID, []blockmanifest.Part{{Candidate: candidate, Size: counter.bytes}})
		}
		query, args := sql.Dialect(tx.Client().Dialect()).Insert(uploadblock.Table).
			Columns(uploadblock.FieldUploadId, uploadblock.FieldBlockHash, uploadblock.FieldCandidateId, uploadblock.FieldSizeBytes).
			Values(session.ID, hashBlockID(blockID), candidate.String(), counter.bytes).
			OnConflict(
				sql.ConflictColumns(uploadblock.FieldUploadId, uploadblock.FieldBlockHash),
				sql.ResolveWith(func(update *sql.UpdateSet) {
					update.SetExcluded(uploadblock.FieldCandidateId).SetExcluded(uploadblock.FieldSizeBytes)
				}),
			).Query()
		if _, err := tx.ExecContext(receiptCtx, query, args...); err != nil {
			return fmt.Errorf("record uploaded block receipt: %w", err)
		}
		return nil
	})
}

func (s *Service) rescheduleCandidateGC(ctx context.Context, folder string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), uploadActivityRenewalTimeout)
	defer cancel()
	now := s.now().UnixMilli()
	_, err := s.db.StorageManifest.Update().Where(
		storagemanifest.HasLocationWith(storagelocation.FolderName(folder)),
		storagemanifest.Or(storagemanifest.CandidateGCAtIsNil(), storagemanifest.CandidateGCAtLT(now)),
	).SetCandidateGCAt(now).Save(cleanupCtx)
	if err != nil {
		s.logger.Warn().Err(err).Str("folder", folder).Msg("reschedule manifest candidate cleanup after failed block upload")
	}
}

func (s *Service) commitManifestBlocks(ctx context.Context, uploadID int64, blockIDs []string) error {
	hashes := make([]string, len(blockIDs))
	unique := make([]string, 0, len(blockIDs))
	receipts := make(map[string]blockmanifest.Part, len(blockIDs))
	for position, blockID := range blockIDs {
		hash := hashBlockID(blockID)
		hashes[position] = hash
		if _, seen := receipts[hash]; !seen {
			unique = append(unique, hash)
			receipts[hash] = blockmanifest.Part{}
		}
	}
	return s.withManifestUploadLock(ctx, uploadID, func(tx *ent.Tx) error {
		const receiptBatchSize = 500
		for start := 0; start < len(unique); start += receiptBatchSize {
			blocks, err := tx.UploadBlock.Query().Where(
				uploadblock.UploadId(uploadID),
				uploadblock.BlockHashIn(unique[start:min(start+receiptBatchSize, len(unique))]...),
			).All(ctx)
			if err != nil {
				return fmt.Errorf("read uploaded block receipts: %w", err)
			}
			for _, block := range blocks {
				candidate, err := uuid.Parse(block.CandidateId)
				if err != nil || candidate == uuid.Nil {
					return fmt.Errorf("invalid uploaded block candidate %q", block.CandidateId)
				}
				receipts[block.BlockHash] = blockmanifest.Part{Candidate: candidate, Size: block.SizeBytes}
			}
		}
		parts := make([]blockmanifest.Part, len(hashes))
		var total int64
		for position, hash := range hashes {
			part := receipts[hash]
			if part.Candidate == uuid.Nil {
				return fmt.Errorf("%w: missing block %d", ErrPartCountMismatch, position)
			}
			if part.Size < 0 || part.Size > math.MaxInt64-total {
				return fmt.Errorf("%w: invalid block list size", ErrPartCountMismatch)
			}
			total += part.Size
			parts[position] = part
		}
		return saveUploadManifest(ctx, tx, uploadID, parts)
	})
}

func saveUploadManifest(ctx context.Context, tx *ent.Tx, uploadID int64, parts []blockmanifest.Part) error {
	data, err := blockmanifest.Encode(parts)
	if err != nil {
		return err
	}
	query, args := sql.Dialect(tx.Client().Dialect()).Insert(storagemanifest.Table).
		Columns(storagemanifest.FieldUploadId, storagemanifest.FieldParts).
		Values(uploadID, data).
		OnConflict(
			sql.ConflictColumns(storagemanifest.FieldUploadId),
			sql.ResolveWith(func(update *sql.UpdateSet) { update.SetExcluded(storagemanifest.FieldParts) }),
		).Query()
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("save upload block manifest: %w", err)
	}
	return nil
}

func (s *Service) withManifestUploadLock(ctx context.Context, uploadID int64, operation func(*ent.Tx) error) error {
	tx, err := s.db.Tx(ctx)
	if err != nil {
		return fmt.Errorf("start block manifest transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockManifestUpload(ctx, tx, uploadID); err != nil {
		return err
	}
	if err := operation(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit block manifest transaction: %w", err)
	}
	return nil
}

func (s *Service) lockManifestUpload(ctx context.Context, tx *ent.Tx, uploadID int64) error {
	affected, err := tx.Upload.Update().Where(upload.ID(uploadID), upload.BlockManifestVersionGT(0)).
		AddBlockManifestVersion(1).SetLastPartUploadedAt(s.now().UnixMilli()).Save(ctx)
	if err != nil {
		return fmt.Errorf("lock manifest upload: %w", err)
	}
	if affected == 0 {
		return ErrUploadNotFound
	}
	return nil
}
