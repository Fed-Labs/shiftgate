package checkpoint

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"shift.dev/shift/internal/objectstore"
)

// failingObjectStore is an object store that cannot store anything: every
// write and read fails, so the mirror step of a fork or live checkpoint is
// guaranteed to fail after the checkpoint is already persisted locally.
type failingObjectStore struct{}

func (failingObjectStore) Put(context.Context, string, io.Reader, int64, string) (objectstore.ObjectInfo, error) {
	return objectstore.ObjectInfo{}, errors.New("object store unavailable")
}

func (failingObjectStore) Get(context.Context, string, io.Writer) (objectstore.ObjectInfo, error) {
	return objectstore.ObjectInfo{}, errors.New("object store unavailable")
}

func (failingObjectStore) Head(context.Context, string) (objectstore.ObjectInfo, error) {
	return objectstore.ObjectInfo{}, objectstore.ErrNotFound
}

func (failingObjectStore) Delete(context.Context, string) error {
	return errors.New("object store unavailable")
}

func (failingObjectStore) InitiateMultipart(context.Context, string, int64, string) (objectstore.MultipartUpload, error) {
	return objectstore.MultipartUpload{}, errors.New("object store unavailable")
}

func (failingObjectStore) UploadPart(context.Context, string, int, io.Reader, int64, string) (objectstore.PartInfo, error) {
	return objectstore.PartInfo{}, errors.New("object store unavailable")
}

func (failingObjectStore) ListParts(context.Context, string) ([]objectstore.PartInfo, error) {
	return nil, errors.New("object store unavailable")
}

func (failingObjectStore) CompleteMultipart(context.Context, string, []objectstore.PartInfo) (objectstore.ObjectInfo, error) {
	return objectstore.ObjectInfo{}, errors.New("object store unavailable")
}

func (failingObjectStore) AbortMultipart(context.Context, string) error {
	return errors.New("object store unavailable")
}

func (failingObjectStore) ListMultipart(context.Context, string) ([]objectstore.MultipartUpload, error) {
	return nil, errors.New("object store unavailable")
}

func (failingObjectStore) CleanupMultipart(context.Context, string, time.Time) (int, error) {
	return 0, errors.New("object store unavailable")
}

// TestForkMirrorFailureRemovesSavedCheckpoint pins the orphan regression: a
// mirror failure after the fork checkpoint was saved must still leave no
// checkpoint behind — the id is recorded before the mirror runs, so the
// rollback that follows removes what was saved.
func TestForkMirrorFailureRemovesSavedCheckpoint(t *testing.T) {
	fixture := newForkFixture(t, fakeEngine{})
	// The fork point is taken before the mirror breaks, so only the fork's
	// own checkpoint meets the failing object store.
	point, err := fixture.service.Create(context.Background(), fixture.source.Spec.ID, CreateOptions{Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.SetMirror(NewMirror(failingObjectStore{}, fixture.chunks, fixture.repo))
	record, err := fixture.forker.Fork(context.Background(), fixture.source.Spec.ID, ForkOptions{Name: "fork-unmirrored", CheckpointID: point.ID})
	if err == nil {
		t.Fatal("a failed object-store mirror must fail the fork")
	}
	if !strings.Contains(err.Error(), "object-store mirror failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	if record.ForkCheckpointID == "" {
		t.Fatal("the locally saved checkpoint id must be recorded before the mirror runs")
	}
	if record.State != ForkRolledBack {
		t.Fatalf("fork state %s, want %s", record.State, ForkRolledBack)
	}
	if _, loadErr := fixture.repo.Load(record.ForkCheckpointID); loadErr == nil {
		t.Fatal("the rollback left an unattached checkpoint in the repository")
	}
	if _, statErr := os.Stat(record.RootPath); !os.IsNotExist(statErr) {
		t.Fatalf("fork root survived rollback: %v", statErr)
	}
}

// TestLiveFinalizeMirrorFailureReturnsSavedManifest pins the live-side orphan
// regression: once the manifest is saved, a mirror failure must return the
// saved manifest with the error so the caller can mark or reap the checkpoint
// instead of losing its id.
func TestLiveFinalizeMirrorFailureReturnsSavedManifest(t *testing.T) {
	fixture := newLiveSessionFixture(t, fakeEngine{})
	fixture.service.SetMirror(NewMirror(failingObjectStore{}, fixture.chunks, fixture.repository))
	live, err := fixture.service.BeginLive(context.Background(), fixture.workloadID, LiveOptions{Passes: 2, LeaveRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	manifest, err := live.Finalize(context.Background())
	if err == nil {
		t.Fatal("a failed object-store mirror must fail the finalize")
	}
	if !strings.Contains(err.Error(), "object-store mirror failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	if manifest.ID == "" {
		t.Fatal("the saved checkpoint id must travel with the mirror error")
	}
	saved, loadErr := fixture.repository.Load(manifest.ID)
	if loadErr != nil {
		t.Fatalf("the checkpoint must be persisted locally: %v", loadErr)
	}
	if saved.ID != manifest.ID {
		t.Fatalf("saved checkpoint %s, want %s", saved.ID, manifest.ID)
	}
}
