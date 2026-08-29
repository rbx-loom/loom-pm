package maintain

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rbx-loom/loom-pm/internal/storage"
)

type fakeStore struct {
	publications []Publication
	err          error
}

func (f *fakeStore) Checksums(context.Context) (map[storage.Digest]struct{}, error) {
	if f.err != nil {
		return nil, f.err
	}

	live := make(map[storage.Digest]struct{}, len(f.publications))
	for _, published := range f.publications {
		live[published.Checksum] = struct{}{}
	}

	return live, nil
}

func (f *fakeStore) EachPublication(_ context.Context, fn func(Publication) error) error {
	if f.err != nil {
		return f.err
	}

	for _, published := range f.publications {
		if err := fn(published); err != nil {
			return err
		}
	}

	return nil
}

// store puts content in the blob store and answers a publication that references it.
func store(t *testing.T, blobs *storage.Filesystem, name, version, content string) Publication {
	t.Helper()

	digest, err := blobs.Put(context.Background(), []byte(content))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	return Publication{Name: name, Version: version, Checksum: digest}
}

// age backdates a blob, so a sweep sees it as older than the grace period.
func age(t *testing.T, root string, digest storage.Digest, when time.Time) {
	t.Helper()

	hex := digest.Hex()
	path := filepath.Join(root, hex[0:2], hex[2:4], hex)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("backdating %s: %v", digest, err)
	}
}

func TestSweepKeepsWhatIsReferenced(t *testing.T) {
	root := t.TempDir()
	blobs := storage.NewFilesystem(root)

	published := store(t, blobs, "serio", "1.0.0", "referenced")
	age(t, root, published.Checksum, time.Now().Add(-30*24*time.Hour))

	report, err := Sweep(context.Background(), blobs, &fakeStore{publications: []Publication{published}},
		time.Hour, time.Now(), true)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if report.Removed != 0 {
		t.Errorf("removed %d blobs, want none: a referenced blob is not an orphan", report.Removed)
	}

	if _, _, err := blobs.Open(context.Background(), published.Checksum); err != nil {
		t.Errorf("the referenced blob is gone: %v", err)
	}
}

func TestSweepRemovesAnOrphan(t *testing.T) {
	root := t.TempDir()
	blobs := storage.NewFilesystem(root)

	orphan := store(t, blobs, "", "", "written by a publish that then failed")
	age(t, root, orphan.Checksum, time.Now().Add(-30*24*time.Hour))

	report, err := Sweep(context.Background(), blobs, &fakeStore{}, time.Hour, time.Now(), true)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if report.Removed != 1 || report.Scanned != 1 {
		t.Fatalf("report = %+v, want one scanned and one removed", report)
	}

	if report.Bytes == 0 {
		t.Error("the report reclaimed no bytes, want the orphan's size")
	}

	if _, _, err := blobs.Open(context.Background(), orphan.Checksum); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("the orphan survived: %v", err)
	}
}

// A blob written seconds ago may belong to a publish whose row is not committed yet.
// Deleting it would break the version that is about to reference it.
func TestSweepSparesARecentOrphan(t *testing.T) {
	blobs := storage.NewFilesystem(t.TempDir())

	orphan := store(t, blobs, "", "", "mid-publish")

	report, err := Sweep(context.Background(), blobs, &fakeStore{}, time.Hour, time.Now(), true)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if report.Removed != 0 {
		t.Fatalf("removed %d blobs, want the grace period to spare it", report.Removed)
	}

	if report.Spared != 1 {
		t.Errorf("report = %+v, want one spared", report)
	}

	if _, _, err := blobs.Open(context.Background(), orphan.Checksum); err != nil {
		t.Errorf("a blob inside the grace period was removed: %v", err)
	}
}

// Deleting data is opt-in: a sweep that has not been asked to remove anything reports what
// it would do and touches nothing.
func TestSweepReportsWithoutRemoving(t *testing.T) {
	root := t.TempDir()
	blobs := storage.NewFilesystem(root)

	orphan := store(t, blobs, "", "", "orphan")
	age(t, root, orphan.Checksum, time.Now().Add(-30*24*time.Hour))

	report, err := Sweep(context.Background(), blobs, &fakeStore{}, time.Hour, time.Now(), false)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if report.Orphaned != 1 {
		t.Errorf("report = %+v, want the orphan counted", report)
	}

	if report.Removed != 0 {
		t.Errorf("report = %+v, want nothing removed", report)
	}

	if _, _, err := blobs.Open(context.Background(), orphan.Checksum); err != nil {
		t.Errorf("a blob was removed by a sweep that was not asked to: %v", err)
	}
}

// If the live set cannot be read, every blob looks like an orphan. Sweeping on that would
// empty the store.
func TestSweepRefusesWithoutTheLiveSet(t *testing.T) {
	root := t.TempDir()
	blobs := storage.NewFilesystem(root)

	published := store(t, blobs, "serio", "1.0.0", "referenced")
	age(t, root, published.Checksum, time.Now().Add(-30*24*time.Hour))

	broken := &fakeStore{err: errors.New("connection refused")}

	if _, err := Sweep(context.Background(), blobs, broken, time.Hour, time.Now(), true); err == nil {
		t.Fatal("Sweep succeeded without knowing what is referenced")
	}

	if _, _, err := blobs.Open(context.Background(), published.Checksum); err != nil {
		t.Errorf("a referenced blob was removed after the live set failed: %v", err)
	}
}

func TestVerifyPasses(t *testing.T) {
	blobs := storage.NewFilesystem(t.TempDir())

	publications := []Publication{
		store(t, blobs, "serio", "1.0.0", "one"),
		store(t, blobs, "math", "2.0.0", "two"),
	}

	report, err := Verify(context.Background(), blobs, &fakeStore{publications: publications})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if report.Checked != 2 || len(report.Missing) != 0 || len(report.Corrupt) != 0 {
		t.Errorf("report = %+v, want two checked and nothing wrong", report)
	}

	if !report.Sound() {
		t.Error("Sound() is false for a store with nothing wrong")
	}
}

// A version row whose blob is gone is a package nobody can install, and the registry
// cannot tell until somebody tries.
func TestVerifyFindsAMissingBlob(t *testing.T) {
	blobs := storage.NewFilesystem(t.TempDir())

	published := store(t, blobs, "serio", "1.0.0", "one")
	if err := blobs.Delete(context.Background(), published.Checksum); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	report, err := Verify(context.Background(), blobs, &fakeStore{publications: []Publication{published}})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if len(report.Missing) != 1 || report.Missing[0].Name != "serio" {
		t.Errorf("report = %+v, want serio reported missing", report)
	}

	if report.Sound() {
		t.Error("Sound() is true for a store missing a blob")
	}
}

// The checksum is what a client verifies against, so a blob that no longer hashes to it is
// worse than one that is gone: the client would install it.
func TestVerifyFindsCorruption(t *testing.T) {
	root := t.TempDir()
	blobs := storage.NewFilesystem(root)

	published := store(t, blobs, "serio", "1.0.0", "the published bytes")

	hex := published.Checksum.Hex()
	path := filepath.Join(root, hex[0:2], hex[2:4], hex)
	if err := os.WriteFile(path, []byte("something else entirely"), 0o644); err != nil {
		t.Fatalf("corrupting the blob: %v", err)
	}

	report, err := Verify(context.Background(), blobs, &fakeStore{publications: []Publication{published}})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if len(report.Corrupt) != 1 || report.Corrupt[0].Version != "1.0.0" {
		t.Errorf("report = %+v, want serio 1.0.0 reported corrupt", report)
	}

	if report.Sound() {
		t.Error("Sound() is true for a store holding a corrupt blob")
	}
}
