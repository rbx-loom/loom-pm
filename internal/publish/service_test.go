package publish

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rbx-loom/loom-pm/internal/manifest"
	"github.com/rbx-loom/loom-pm/internal/storage"
)

type fakeStore struct {
	recorded      []Record
	recordErr     error
	unsatisfiable []string
	lookupErr     error
}

func (f *fakeStore) Record(_ context.Context, record Record) error {
	if f.recordErr != nil {
		return f.recordErr
	}

	f.recorded = append(f.recorded, record)
	return nil
}

func (f *fakeStore) Unsatisfiable(_ context.Context, dependencies []manifest.Dependency) ([]manifest.Dependency, error) {
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}

	var missing []manifest.Dependency
	for _, dependency := range dependencies {
		for _, name := range f.unsatisfiable {
			if dependency.Name.String() == name {
				missing = append(missing, dependency)
			}
		}
	}

	return missing, nil
}

func service(t *testing.T, store *fakeStore) (*Service, storage.Blobs) {
	t.Helper()

	blobs := storage.NewFilesystem(t.TempDir())
	return NewService(store, blobs, DefaultLimits()), blobs
}

func TestPublishStoresAndRecords(t *testing.T) {
	store := &fakeStore{}
	publisher, blobs := service(t, store)

	content := valid(t)
	payload, err := publisher.Publish(context.Background(), content, 7)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if len(store.recorded) != 1 {
		t.Fatalf("recorded %d publications, want 1", len(store.recorded))
	}

	recorded := store.recorded[0]
	if recorded.PublisherID != 7 {
		t.Errorf("recorded publisher %d, want 7", recorded.PublisherID)
	}

	if recorded.Payload.Manifest.Package.Name.String() != "serio" {
		t.Errorf("recorded %q, want serio", recorded.Payload.Manifest.Package.Name)
	}

	reader, size, err := blobs.Open(context.Background(), payload.Digest)
	if err != nil {
		t.Fatalf("the tarball was not stored: %v", err)
	}
	defer reader.Close()

	if size != int64(len(content)) {
		t.Errorf("stored %d bytes, want %d", size, len(content))
	}
}

func TestPublishRejectsAnUnreadableUpload(t *testing.T) {
	store := &fakeStore{}
	publisher, _ := service(t, store)

	if _, err := publisher.Publish(context.Background(), []byte("not a tarball"), 7); err == nil {
		t.Fatal("Publish accepted something that is not a tarball")
	}

	if len(store.recorded) != 0 {
		t.Error("an unreadable upload was recorded")
	}
}

// A version depending on something no published version satisfies can never resolve, so
// publishing it only produces a broken package for whoever tries.
func TestPublishRejectsAnUnsatisfiableDependency(t *testing.T) {
	store := &fakeStore{unsatisfiable: []string{"math"}}
	publisher, _ := service(t, store)

	_, err := publisher.Publish(context.Background(), valid(t), 7)
	if err == nil {
		t.Fatal("Publish accepted a version whose dependency cannot resolve")
	}

	if !strings.Contains(err.Error(), "math") {
		t.Errorf("the diagnostic %q does not name the dependency", err)
	}

	if len(store.recorded) != 0 {
		t.Error("an unresolvable version was recorded")
	}
}

// A dev dependency is what a package's own tests are written against and no part of
// compiling it for someone else, so it is not resolution's business and not the
// registry's either.
func TestPublishIgnoresDevDependencies(t *testing.T) {
	store := &fakeStore{unsatisfiable: []string{"runit"}}
	publisher, _ := service(t, store)

	content := archive(t,
		file("loom-config.toml", `
[package]
name = "serio"
version = "1.2.0"

[dependencies]
runit = { version = "^0.4", dev = true }
`),
		file("src/init.loom", "fn main() {}"),
	)

	if _, err := publisher.Publish(context.Background(), content, 7); err != nil {
		t.Fatalf("Publish refused a package whose only unsatisfiable dependency is a dev one: %v", err)
	}
}

func TestPublishPropagatesARefusal(t *testing.T) {
	store := &fakeStore{recordErr: ErrAlreadyPublished}
	publisher, _ := service(t, store)

	_, err := publisher.Publish(context.Background(), valid(t), 7)
	if !errors.Is(err, ErrAlreadyPublished) {
		t.Errorf("Publish = %v, want ErrAlreadyPublished", err)
	}
}

// The blob goes in before the row, because a blob with no row is garbage to sweep while a
// row with no blob is a package nobody can install.
func TestPublishStoresTheBlobBeforeRecording(t *testing.T) {
	store := &fakeStore{recordErr: errors.New("the database went away")}
	publisher, blobs := service(t, store)

	content := valid(t)
	if _, err := publisher.Publish(context.Background(), content, 7); err == nil {
		t.Fatal("Publish succeeded despite the store failing")
	}

	reader, _, err := blobs.Open(context.Background(), storage.DigestOf(content))
	if err != nil {
		t.Fatalf("the tarball was not stored before the row was attempted: %v", err)
	}
	reader.Close()
}
