package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Filesystem stores blobs under root, fanned out two levels by the leading bytes of the
// digest so no single directory holds every package in the registry.
type Filesystem struct {
	root string
}

func NewFilesystem(root string) *Filesystem {
	return &Filesystem{root: root}
}

func (f *Filesystem) Put(_ context.Context, content []byte) (Digest, error) {
	digest := DigestOf(content)
	path := f.pathOf(digest)

	if _, err := os.Stat(path); err == nil {
		return digest, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Digest{}, fmt.Errorf("storage: checking for %s: %w", digest, err)
	}

	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return Digest{}, fmt.Errorf("storage: creating %s: %w", directory, err)
	}

	// written under a temporary name and renamed, so a blob is never visible under the
	// name its digest promises until all of it is there
	temporary, err := os.CreateTemp(directory, ".partial-*")
	if err != nil {
		return Digest{}, fmt.Errorf("storage: creating a temporary file in %s: %w", directory, err)
	}
	defer os.Remove(temporary.Name())

	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return Digest{}, fmt.Errorf("storage: writing %s: %w", digest, err)
	}

	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return Digest{}, fmt.Errorf("storage: flushing %s: %w", digest, err)
	}

	if err := temporary.Close(); err != nil {
		return Digest{}, fmt.Errorf("storage: closing %s: %w", digest, err)
	}

	if err := os.Rename(temporary.Name(), path); err != nil {
		return Digest{}, fmt.Errorf("storage: publishing %s: %w", digest, err)
	}

	// the rename is not durable until the directory entry is: a crash here would leave a
	// version row pointing at a blob that never arrived, which is the state the
	// write-then-rename was chosen to avoid
	handle, err := os.Open(directory)
	if err != nil {
		return Digest{}, fmt.Errorf("storage: opening %s to flush it: %w", directory, err)
	}
	defer handle.Close()

	if err := handle.Sync(); err != nil {
		return Digest{}, fmt.Errorf("storage: flushing %s: %w", directory, err)
	}

	return digest, nil
}

func (f *Filesystem) Open(_ context.Context, digest Digest) (io.ReadCloser, int64, error) {
	file, err := os.Open(f.pathOf(digest))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, fmt.Errorf("%s: %w", digest, ErrNotFound)
	} else if err != nil {
		return nil, 0, fmt.Errorf("storage: opening %s: %w", digest, err)
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, 0, fmt.Errorf("storage: measuring %s: %w", digest, err)
	}

	return file, info.Size(), nil
}

func (f *Filesystem) pathOf(digest Digest) string {
	encoded := digest.Hex()
	return filepath.Join(f.root, encoded[0:2], encoded[2:4], encoded)
}
