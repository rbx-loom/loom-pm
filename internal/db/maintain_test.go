package db

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/rbx-loom/loom-pm/internal/maintain"
	"github.com/rbx-loom/loom-pm/internal/storage"
)

func TestChecksums(t *testing.T) {
	pool := testPool(t)
	seed(t, pool, "serio", "1.0.0", "1.2.0")
	seed(t, pool, "scope/math", "2.0.0")

	live, err := NewStore(pool).Checksums(context.Background())
	if err != nil {
		t.Fatalf("Checksums: %v", err)
	}

	if len(live) != 3 {
		t.Fatalf("read %d checksums, want 3", len(live))
	}

	// seed derives each blob's content from the version string
	for _, version := range []string{"1.0.0", "1.2.0", "2.0.0"} {
		if _, ok := live[storage.DigestOf([]byte(version))]; !ok {
			t.Errorf("the checksum for %s is not in the live set", version)
		}
	}
}

// A yanked version is still installable by a lock file that pins it, so its blob is still
// referenced and must not be swept.
func TestChecksumsIncludesYankedVersions(t *testing.T) {
	pool := testPool(t)
	seed(t, pool, "serio", "1.0.0")

	_, err := pool.Exec(context.Background(), `UPDATE versions SET yanked_at = now()`)
	if err != nil {
		t.Fatalf("yanking: %v", err)
	}

	live, err := NewStore(pool).Checksums(context.Background())
	if err != nil {
		t.Fatalf("Checksums: %v", err)
	}

	if len(live) != 1 {
		t.Errorf("read %d checksums, want the yanked version still counted", len(live))
	}
}

func TestChecksumsIsEmptyForAnEmptyRegistry(t *testing.T) {
	pool := testPool(t)

	live, err := NewStore(pool).Checksums(context.Background())
	if err != nil {
		t.Fatalf("Checksums: %v", err)
	}

	if len(live) != 0 {
		t.Errorf("read %d checksums from an empty registry", len(live))
	}
}

func TestEachPublication(t *testing.T) {
	pool := testPool(t)
	seed(t, pool, "serio", "1.0.0", "1.2.0-beta.1")
	seed(t, pool, "scope/math", "2.0.0")

	var listed []string
	err := NewStore(pool).EachPublication(context.Background(), func(published maintain.Publication) error {
		listed = append(listed, published.String())

		if published.Checksum != storage.DigestOf([]byte(versionOf(published))) {
			t.Errorf("%s carries the wrong checksum", published)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("EachPublication: %v", err)
	}

	sort.Strings(listed)
	want := []string{"scope/math 2.0.0", "serio 1.0.0", "serio 1.2.0-beta.1"}

	if len(listed) != len(want) {
		t.Fatalf("listed %v, want %v", listed, want)
	}

	for index, expected := range want {
		if listed[index] != expected {
			t.Errorf("listed[%d] = %q, want %q", index, listed[index], expected)
		}
	}
}

// versionOf is what seed hashed to make the blob: the version string alone.
func versionOf(published maintain.Publication) string {
	return published.Version
}

func TestEachPublicationStops(t *testing.T) {
	pool := testPool(t)
	seed(t, pool, "serio", "1.0.0", "1.2.0", "1.3.0")

	stop := errors.New("enough")
	var visited int

	err := NewStore(pool).EachPublication(context.Background(), func(maintain.Publication) error {
		visited++
		return stop
	})

	if !errors.Is(err, stop) {
		t.Errorf("EachPublication = %v, want the callback's error", err)
	}

	if visited != 1 {
		t.Errorf("visited %d publications after the callback stopped, want 1", visited)
	}
}
