package blockmanifest

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"

	"github.com/NCUnix/GitHubActionCacheServer/internal/ent"
	"github.com/NCUnix/GitHubActionCacheServer/internal/ent/storagemanifest"
	"github.com/NCUnix/GitHubActionCacheServer/internal/storage"
	"github.com/google/uuid"
)

const partBytes = 16 + 8

type Part struct {
	Candidate uuid.UUID
	Size      int64
}

func Folder(folder string) string {
	return storage.PartsFolder(folder) + "/v2"
}

func Object(folder, candidate string) string {
	return Folder(folder) + "/" + candidate + "/data"
}

func Encode(parts []Part) ([]byte, error) {
	if len(parts) == 0 || len(parts) > storage.MaxIndexedObjects {
		return nil, fmt.Errorf("invalid manifest part count %d", len(parts))
	}
	data := make([]byte, 1, 1+len(parts)*partBytes)
	data[0] = 1
	for _, part := range parts {
		data = append(data, part.Candidate[:]...)
		data = binary.BigEndian.AppendUint64(data, uint64(part.Size))
	}
	return data, nil
}

func Decode(data []byte) ([]Part, int64, error) {
	if len(data) <= 1 || data[0] != 1 || (len(data)-1)%partBytes != 0 || (len(data)-1)/partBytes > storage.MaxIndexedObjects {
		return nil, 0, fmt.Errorf("invalid storage manifest format")
	}
	parts := make([]Part, (len(data)-1)/partBytes)
	var total int64
	for position := range parts {
		offset := 1 + position*partBytes
		part := &parts[position]
		copy(part.Candidate[:], data[offset:offset+16])
		part.Size = int64(binary.BigEndian.Uint64(data[offset+16 : offset+partBytes]))
		if part.Candidate == uuid.Nil || part.Size < 0 || part.Size > math.MaxInt64-total {
			return nil, 0, fmt.Errorf("invalid storage manifest part %d", position)
		}
		total += part.Size
	}
	return parts, total, nil
}

func ForLocation(ctx context.Context, db *ent.Client, location *ent.StorageLocation) ([]Part, int64, error) {
	manifest, err := db.StorageManifest.Query().Where(storagemanifest.LocationId(location.ID)).Only(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("read storage manifest: %w", err)
	}
	return ForRecord(manifest, location)
}

func ForRecord(manifest *ent.StorageManifest, location *ent.StorageLocation) ([]Part, int64, error) {
	parts, total, err := Decode(manifest.Parts)
	if err != nil {
		return nil, 0, err
	}
	if location.ManifestAnchor == nil || len(parts) != location.PartCount || parts[0].Candidate.String() != *location.ManifestAnchor ||
		(location.SizeBytes != nil && *location.SizeBytes != total) {
		return nil, 0, fmt.Errorf("storage manifest disagrees with location %s", location.ID)
	}
	return parts, total, nil
}
