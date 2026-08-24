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
