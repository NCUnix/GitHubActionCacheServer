package blockmanifest

import (
	"testing"

	"github.com/NCUnix/GitHubActionCacheServer/internal/storage"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestMaximumManifestPayload(t *testing.T) {
	parts := make([]Part, storage.MaxIndexedObjects)
	part := Part{Candidate: uuid.New(), Size: 1}
	for position := range parts {
		parts[position] = part
	}
	payload, err := Encode(parts)
	require.NoError(t, err)
	require.Len(t, payload, 1_200_001)
	decoded, total, err := Decode(payload)
	require.NoError(t, err)
	require.Equal(t, parts, decoded)
	require.Equal(t, int64(storage.MaxIndexedObjects), total)
	_, _, err = Decode(payload[:len(payload)-1])
	require.Error(t, err)
}
