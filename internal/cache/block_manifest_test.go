package cache

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/NCUnix/GitHubActionCacheServer/internal/blockmanifest"
	"github.com/NCUnix/GitHubActionCacheServer/internal/cleanup"
	"github.com/NCUnix/GitHubActionCacheServer/internal/config"
	"github.com/NCUnix/GitHubActionCacheServer/internal/storage"
	"github.com/NCUnix/GitHubActionCacheServer/internal/storagelifecycle"
	"github.com/NCUnix/GitHubActionCacheServer/internal/storageoutbox"
	"github.com/NCUnix/GitHubActionCacheServer/internal/storagereconcile"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestManifestUploadKeepsCommittedOrderAndPinnedLayout(t *testing.T) {
	ctx, client, filesystem := newTestServiceDeps(t)
	scope := writableScope()
	legacy := newTestService(t, Options{DB: client, Storage: filesystem})
	legacySession, err := legacy.CreateUpload(ctx, "legacy", "v", scope)
	require.NoError(t, err)

	adapter := &manifestTestStorage{Adapter: filesystem}
	writer := newTestService(t, Options{DB: client, Storage: adapter})
	reader := newTestService(t, Options{DB: client, Storage: filesystem})
	session, err := writer.CreateUpload(ctx, "manifest", "v", scope)
	require.NoError(t, err)
	require.Positive(t, client.Upload.GetX(ctx, session.UploadID).BlockManifestVersion)

	require.NoError(t, writer.UploadBlock(ctx, legacySession.UploadID, "old", strings.NewReader("old")))
	require.NoError(t, writer.CommitBlockList(ctx, legacySession.UploadID, []string{"old"}))
	_, err = writer.CompleteUpload(ctx, "legacy", "v", scope)
	require.NoError(t, err)
	require.Equal(t, 1, adapter.copies)

	require.NoError(t, writer.UploadBlock(ctx, session.UploadID, "ab+c", strings.NewReader("stale")))
	require.NoError(t, reader.UploadBlock(ctx, session.UploadID, "ab+c", strings.NewReader("ABCD")))
	require.NoError(t, writer.UploadBlock(ctx, session.UploadID, "Ab+c", strings.NewReader("xyz")))
	order := []string{"Ab+c", "ab+c", "Ab+c"}
	require.NoError(t, writer.CommitBlockList(ctx, session.UploadID, order))
	require.NoError(t, reader.CommitBlockList(ctx, session.UploadID, order))
	require.NoError(t, reader.UploadBlock(ctx, session.UploadID, "Ab+c", strings.NewReader("new")))
	require.ErrorIs(t, reader.CommitBlockList(ctx, session.UploadID, []string{"missing"}), ErrPartCountMismatch)
	_, err = reader.CompleteUpload(ctx, "manifest", "v", scope)
	require.NoError(t, err)
	require.Equal(t, 1, adapter.copies)
	require.Zero(t, client.UploadBlock.Query().CountX(ctx))

	match, err := reader.GetCacheEntryWithDownloadURL(ctx, []string{"manifest"}, "v", scope, func(id string) string { return id })
	require.NoError(t, err)
	require.NotNil(t, match)
	cacheID := match.CacheEntry.ID
	location := client.StorageLocation.GetX(ctx, match.CacheEntry.LocationId)
	require.NotNil(t, location.ManifestAnchor)
	require.Equal(t, int64(10), *location.SizeBytes)
	metadata, err := reader.DownloadMetadata(ctx, cacheID)
	require.NoError(t, err)
	require.Equal(t, int64(10), metadata.ContentLength)
	full, err := reader.Download(ctx, cacheID)
	require.NoError(t, err)
	require.Equal(t, "xyzABCDxyz", readManifestStream(t, full))
	ranged, err := reader.DownloadRanged(ctx, cacheID, []ByteRange{ClosedByteRange(2, 7)})
	require.NoError(t, err)
	require.Equal(t, "zABCDx", readManifestStream(t, ranged))

	client.StorageLocation.UpdateOneID(location.ID).ClearSizeBytes().SaveX(ctx)
	result, err := storagereconcile.Reconcile(ctx, storagereconcile.Options{DB: client, Storage: filesystem, Lifecycle: reader.lifecycle})
	require.NoError(t, err)
	require.Equal(t, 1, result.Updated)
	require.Equal(t, int64(10), *client.StorageLocation.GetX(ctx, location.ID).SizeBytes)
}

func TestManifestCandidateCleanupRearmsAfterLatePUT(t *testing.T) {
	ctx, client, filesystem := newTestServiceDeps(t)
	now := time.Now().Truncate(time.Second)
	scope := writableScope()
	adapter := &manifestTestStorage{Adapter: filesystem, objects: make(map[string]time.Time)}
	writer := newTestService(t, Options{DB: client, Storage: adapter, Now: func() time.Time { return now }})
	session, err := writer.CreateUpload(ctx, "manifest", "v", scope)
	require.NoError(t, err)
	folder := client.Upload.GetX(ctx, session.UploadID).FolderName
	require.NoError(t, writer.UploadBlock(ctx, session.UploadID, "block", strings.NewReader("old")))
	require.NoError(t, writer.UploadBlock(ctx, session.UploadID, "block", strings.NewReader("published")))
	require.NoError(t, writer.CommitBlockList(ctx, session.UploadID, []string{"block"}))

	started, release := make(chan struct{}), make(chan struct{})
	lateStorage := &blockedManifestStorage{Adapter: filesystem, started: started, release: release}
	replica := newTestService(t, Options{DB: client, Storage: lateStorage, Now: func() time.Time { return now }, UploadHeartbeatInterval: time.Hour})
	result := make(chan error, 1)
	go func() {
		result <- replica.UploadBlock(ctx, session.UploadID, "block", strings.NewReader("late"))
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("PUT never entered storage")
	}
	defer func() {
		select {
		case <-release:
		default:
			close(release)
			<-result
		}
	}()
	require.NoError(t, writer.UploadBlock(ctx, session.UploadID, "unused", strings.NewReader("unused")))
	_, err = writer.CompleteUpload(ctx, "manifest", "v", scope)
	require.NoError(t, err)
	location := client.StorageLocation.Query().OnlyX(ctx)
	firstGC := client.StorageManifest.Query().OnlyX(ctx).CandidateGCAt
	require.NotNil(t, firstGC)

	sweeper := cleanup.NewService(cleanup.Options{
		DB: client, Storage: adapter, Now: func() time.Time { return now },
		Config: config.CleanupConfig{OrphanedStorageGracePeriod: time.Hour},
	})
	deleted, err := sweeper.RunManifestCandidates(ctx)
	require.NoError(t, err)
	require.Zero(t, deleted)
	now = now.Add(2 * time.Hour)
	lease, err := writer.lifecycle.AcquireReader(ctx, client.CacheEntry.Query().OnlyIDX(ctx), storagelifecycle.AcquireReaderOptions{})
	require.NoError(t, err)
	adapter.pruneErr = errors.New("injected S3 listing failure")
	_, err = sweeper.RunManifestCandidates(ctx)
	require.ErrorIs(t, err, adapter.pruneErr)
	require.NotNil(t, client.StorageManifest.Query().OnlyX(ctx).CandidateGCAt)
	adapter.pruneErr = nil
	deleted, err = sweeper.RunManifestCandidates(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, deleted)
	require.Nil(t, client.StorageManifest.Query().OnlyX(ctx).CandidateGCAt)
	require.Equal(t, "published", readStorageObject(t, ctx, filesystem, anchorObjectName(location)))

	close(release)
	require.ErrorIs(t, <-result, ErrUploadNotFound)
	require.NotNil(t, client.StorageManifest.Query().OnlyX(ctx).CandidateGCAt)
	adapter.objects[lateStorage.name] = now
	young := blockmanifest.Object(folder, uuid.NewString())
	require.NoError(t, filesystem.UploadStream(ctx, young, strings.NewReader("missing receipt")))
	adapter.objects[young] = now.Add(90 * time.Minute)
	now = now.Add(2 * time.Hour)
	deleted, err = sweeper.RunManifestCandidates(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, deleted)
	require.NotNil(t, client.StorageManifest.Query().OnlyX(ctx).CandidateGCAt)
	now = now.Add(time.Hour)
	deleted, err = sweeper.RunManifestCandidates(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, deleted)
	require.Nil(t, client.StorageManifest.Query().OnlyX(ctx).CandidateGCAt)
	require.NoError(t, writer.lifecycle.ReleaseReader(ctx, lease.ID))
}

func TestManifestSinglePUTDirectDownload(t *testing.T) {
	ctx, client, filesystem := newTestServiceDeps(t)
	adapter := &manifestTestStorage{Adapter: filesystem}
	service := newTestService(t, Options{DB: client, Storage: adapter, EnableDirectDownloads: true})
	scope := writableScope()
	session, err := service.CreateUpload(ctx, "key", "v", scope)
	require.NoError(t, err)
	require.NoError(t, service.UploadPart(ctx, session.UploadID, strings.NewReader("one")))
	_, err = service.CompleteUpload(ctx, "key", "v", scope)
	require.NoError(t, err)
	location := client.StorageLocation.Query().OnlyX(ctx)
	match, err := service.GetCacheEntryWithDownloadURL(ctx, []string{"key"}, "v", scope, func(string) string { return "proxy" })
	require.NoError(t, err)
	require.NotNil(t, match)
	require.Equal(t, anchorObjectName(location), match.DownloadURL)
	require.Equal(t, 1, client.StorageReaderLease.Query().CountX(ctx))
}

func TestManifestComposeRespectsOrderAndPartReaderLease(t *testing.T) {
	ctx, client, filesystem := newTestServiceDeps(t)
	adapter := &manifestComposeStorage{&manifestTestStorage{Adapter: filesystem}}
	service := newTestService(t, Options{DB: client, Storage: adapter})
	scope := writableScope()
	session, err := service.CreateUpload(ctx, "key", "v", scope)
	require.NoError(t, err)
	require.NoError(t, service.UploadBlock(ctx, session.UploadID, "first", strings.NewReader("one")))
	require.NoError(t, service.UploadBlock(ctx, session.UploadID, "second", strings.NewReader("two")))
	require.NoError(t, service.CommitBlockList(ctx, session.UploadID, []string{"second", "first", "second"}))
	_, err = service.CompleteUpload(ctx, "key", "v", scope)
	require.NoError(t, err)
	entry := client.CacheEntry.Query().OnlyX(ctx)
	lease, err := service.lifecycle.AcquireReader(ctx, entry.ID, storagelifecycle.AcquireReaderOptions{})
	require.NoError(t, err)
	service.tryStartMaterialization(lease.Location, entry.ID)
	require.NoError(t, service.WaitForMerges(ctx))
	require.Equal(t, "twoonetwo", readStorageObject(t, ctx, filesystem, mergedObjectName(lease.Location.FolderName)))

	result, err := service.lifecycle.ClaimPartsDeletion(ctx, entry.LocationId)
	require.NoError(t, err)
	require.Nil(t, result.Task)
	require.NoError(t, service.lifecycle.ReleaseReader(ctx, lease.ID))
	result, err = service.lifecycle.ClaimPartsDeletion(ctx, entry.LocationId)
	require.NoError(t, err)
	require.NotNil(t, result.Task)
	require.Equal(t, partsFolderName(lease.Location.FolderName), result.Task.FolderName)
	require.NoError(t, storageoutbox.Process(ctx, client, filesystem, result.Task))
	exists, err := filesystem.ObjectExists(ctx, anchorObjectName(lease.Location))
	require.NoError(t, err)
	require.False(t, exists)
	restored, err := service.Download(ctx, entry.ID)
	require.NoError(t, err)
	require.Equal(t, "twoonetwo", readManifestStream(t, restored))
}

func readManifestStream(t *testing.T, stream *DownloadStream) string {
	t.Helper()
	data, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	return string(data)
}

type manifestTestStorage struct {
	storage.Adapter
	copies   int
	pruneErr error
	objects  map[string]time.Time
}

func (adapter *manifestTestStorage) CopyObject(ctx context.Context, source, destination string) error {
	adapter.copies++
	return adapter.Adapter.CopyObject(ctx, source, destination)
}

func (adapter *manifestTestStorage) UploadStream(ctx context.Context, name string, stream io.Reader) error {
	if err := adapter.Adapter.UploadStream(ctx, name, stream); err != nil {
		return err
	}
	if adapter.objects != nil {
		adapter.objects[name] = time.Unix(1, 0)
	}
	return nil
}

func (adapter *manifestTestStorage) CreateDownloadURL(_ context.Context, name string, _ time.Duration) (string, error) {
	return name, nil
}

func (adapter *manifestTestStorage) PruneFolderObjects(ctx context.Context, folder string, remove func(storage.ObjectMetadata) bool) (int, bool, error) {
	if adapter.pruneErr != nil {
		return 0, false, adapter.pruneErr
	}
	deleted := 0
	for name, modified := range adapter.objects {
		relative, withinFolder := strings.CutPrefix(name, folder+"/")
		if !withinFolder || !remove(storage.ObjectMetadata{Name: relative, ModifiedAt: modified}) {
			continue
		}
		if err := adapter.DeleteFolder(ctx, strings.TrimSuffix(name, "/data")); err != nil {
			return deleted, false, err
		}
		delete(adapter.objects, name)
		deleted++
	}
	return deleted, false, nil
}

type blockedManifestStorage struct {
	storage.Adapter
	started, release chan struct{}
	name             string
}

func (adapter *blockedManifestStorage) UploadStream(ctx context.Context, name string, stream io.Reader) error {
	adapter.name = name
	close(adapter.started)
	<-adapter.release
	return adapter.Adapter.UploadStream(ctx, name, stream)
}

func (*blockedManifestStorage) PruneFolderObjects(context.Context, string, func(storage.ObjectMetadata) bool) (int, bool, error) {
	panic("unused")
}

type manifestComposeStorage struct{ *manifestTestStorage }

func (adapter *manifestComposeStorage) ComposeObjects(ctx context.Context, destination string, sources []string) error {
	var body strings.Builder
	for _, name := range sources {
		source, err := adapter.CreateDownloadStream(ctx, name)
		if err != nil {
			return err
		}
		_, err = io.Copy(&body, source)
		if err := errors.Join(err, source.Close()); err != nil {
			return err
		}
	}
	return adapter.UploadStream(ctx, destination, strings.NewReader(body.String()))
}
