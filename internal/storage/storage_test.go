package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDigestRoundTrip(t *testing.T) {
	digest := DigestOf([]byte("package contents"))

	parsed, err := ParseDigest(digest.String())
	if err != nil {
		t.Fatalf("ParseDigest(%q): %v", digest.String(), err)
	}

	if parsed != digest {
		t.Errorf("ParseDigest(%q) = %v, want %v", digest.String(), parsed, digest)
	}

	if !strings.HasPrefix(digest.String(), "sha256:") {
		t.Errorf("String() = %q, want a sha256: prefix", digest.String())
	}
}

func TestDigestOfIsStable(t *testing.T) {
	if DigestOf([]byte("a")) != DigestOf([]byte("a")) {
		t.Error("DigestOf is not stable for equal content")
	}

	if DigestOf([]byte("a")) == DigestOf([]byte("b")) {
		t.Error("DigestOf collides for different content")
	}
}

func TestParseDigestRejects(t *testing.T) {
	cases := []string{
		"",
		"sha256:",
		"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		"sha256:not-hex-not-hex-not-hex-not-hex-not-hex-not-hex-not-hex-not-hex",
		"sha256:9f86d081",
		"md5:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		"sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a0800",
	}

	for _, text := range cases {
		t.Run(text, func(t *testing.T) {
			if _, err := ParseDigest(text); err == nil {
				t.Errorf("ParseDigest(%q) succeeded, want an error", text)
			}
		})
	}
}

func TestFilesystemPutAndOpen(t *testing.T) {
	blobs := NewFilesystem(t.TempDir())
	content := []byte("loom-config.toml and some sources")

	digest, err := blobs.Put(context.Background(), content)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	if digest != DigestOf(content) {
		t.Errorf("Put returned %v, want the digest of the content", digest)
	}

	reader, size, err := blobs.Open(context.Background(), digest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reader.Close()

	if size != int64(len(content)) {
		t.Errorf("Open reported size %d, want %d", size, len(content))
	}

	read, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading the blob: %v", err)
	}

	if !bytes.Equal(read, content) {
		t.Errorf("read %q, want %q", read, content)
	}
}

// A published version is never replaced, so storing the same content twice has to be a
// no-op rather than a conflict — a retried publish must not fail on the blob.
func TestFilesystemPutIsIdempotent(t *testing.T) {
	blobs := NewFilesystem(t.TempDir())
	content := []byte("identical")

	first, err := blobs.Put(context.Background(), content)
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}

	second, err := blobs.Put(context.Background(), content)
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}

	if first != second {
		t.Errorf("Put returned %v then %v for identical content", first, second)
	}
}

func TestFilesystemOpenMissing(t *testing.T) {
	blobs := NewFilesystem(t.TempDir())

	_, _, err := blobs.Open(context.Background(), DigestOf([]byte("never stored")))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Open of a missing blob = %v, want ErrNotFound", err)
	}
}

// Blobs are fanned out two levels so no single directory holds every package in the
// registry.
func TestFilesystemFansOut(t *testing.T) {
	root := t.TempDir()
	blobs := NewFilesystem(root)

	digest, err := blobs.Put(context.Background(), []byte("content"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	hex := digest.Hex()
	path := filepath.Join(root, hex[0:2], hex[2:4], hex)
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected a blob at %s: %v", path, err)
	}
}

// Put must not leave a partial file behind under the final name: a half-written blob
// whose digest says it is complete is the one failure the store must not have.
func TestFilesystemLeavesNoTemporaries(t *testing.T) {
	root := t.TempDir()
	blobs := NewFilesystem(root)

	if _, err := blobs.Put(context.Background(), []byte("content")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	var found int
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			found++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the store: %v", err)
	}

	if found != 1 {
		t.Errorf("store holds %d files, want exactly the blob", found)
	}
}

func TestFilesystemWalk(t *testing.T) {
	blobs := NewFilesystem(t.TempDir())
	ctx := context.Background()

	want := map[Digest]bool{}
	for _, content := range []string{"one", "two", "three"} {
		digest, err := blobs.Put(ctx, []byte(content))
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		want[digest] = true
	}

	seen := map[Digest]bool{}
	err := blobs.Walk(ctx, func(digest Digest, size int64, modified time.Time) error {
		if size == 0 {
			t.Errorf("%s was walked with no size", digest)
		}
		if modified.IsZero() {
			t.Errorf("%s was walked with no modification time", digest)
		}
		seen[digest] = true
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if len(seen) != len(want) {
		t.Fatalf("walked %d blobs, want %d", len(seen), len(want))
	}

	for digest := range want {
		if !seen[digest] {
			t.Errorf("%s was not walked", digest)
		}
	}
}

func TestFilesystemWalkIsEmptyForAnEmptyStore(t *testing.T) {
	blobs := NewFilesystem(t.TempDir())

	err := blobs.Walk(context.Background(), func(Digest, int64, time.Time) error {
		t.Error("an empty store walked a blob")
		return nil
	})
	if err != nil {
		t.Errorf("Walk of an empty store: %v", err)
	}
}

// A file whose name is not a digest is not a blob. Walking must not report one, because
// the sweeper would then delete something it never wrote.
func TestFilesystemWalkIgnoresStrays(t *testing.T) {
	root := t.TempDir()
	blobs := NewFilesystem(root)

	if _, err := blobs.Put(context.Background(), []byte("real")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	stray := filepath.Join(root, "ab", "cd")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatalf("making a stray directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stray, "not-a-digest"), []byte("x"), 0o644); err != nil {
		t.Fatalf("writing a stray: %v", err)
	}

	var walked int
	err := blobs.Walk(context.Background(), func(Digest, int64, time.Time) error {
		walked++
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if walked != 1 {
		t.Errorf("walked %d blobs, want only the real one", walked)
	}
}

func TestFilesystemWalkStops(t *testing.T) {
	blobs := NewFilesystem(t.TempDir())
	ctx := context.Background()

	for _, content := range []string{"one", "two", "three"} {
		if _, err := blobs.Put(ctx, []byte(content)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	stop := errors.New("enough")
	var walked int

	err := blobs.Walk(ctx, func(Digest, int64, time.Time) error {
		walked++
		return stop
	})

	if !errors.Is(err, stop) {
		t.Errorf("Walk = %v, want the callback's error", err)
	}

	if walked != 1 {
		t.Errorf("walked %d blobs after the callback stopped, want 1", walked)
	}
}

func TestFilesystemDelete(t *testing.T) {
	blobs := NewFilesystem(t.TempDir())
	ctx := context.Background()

	digest, err := blobs.Put(ctx, []byte("removable"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := blobs.Delete(ctx, digest); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, _, err := blobs.Open(ctx, digest); !errors.Is(err, ErrNotFound) {
		t.Errorf("Open after Delete = %v, want ErrNotFound", err)
	}

	// deleting what is already gone is the state the caller asked for
	if err := blobs.Delete(ctx, digest); err != nil {
		t.Errorf("Delete of a missing blob: %v", err)
	}
}
