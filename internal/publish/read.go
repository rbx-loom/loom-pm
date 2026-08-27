// Package publish turns an uploaded tarball into a publication.
//
// Every piece of metadata is derived from the loom-config.toml inside the archive and
// none is accepted from the client. LocalPackageIndex derives what a version declares by
// reading that same manifest, and a registry deriving it from a separately submitted blob
// would disagree with it about what a package says.
package publish

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"

	"github.com/rbx-loom/loom-pm/internal/manifest"
	"github.com/rbx-loom/loom-pm/internal/storage"
)

// Limits bound what an upload may cost to accept. A Loom package is source text, so these
// are generous rather than tight: exceeding one is a mistake, not a tradeoff.
type Limits struct {
	CompressedBytes   int64
	DecompressedBytes int64
	FileBytes         int64

	// ManifestBytes bounds loom-config.toml on its own. FileBytes is sized for source a
	// package ships; a manifest that large is a generated file rather than an identity.
	ManifestBytes int64

	Files int
}

func DefaultLimits() Limits {
	return Limits{
		CompressedBytes:   5 << 20,
		DecompressedBytes: 25 << 20,
		FileBytes:         5 << 20,
		ManifestBytes:     64 << 10,
		Files:             2000,
	}
}

// Valid reports whether every bound is set. A partially filled Limits is a configuration
// mistake, not a request to default the rest.
func (l Limits) Valid() bool {
	return l.CompressedBytes > 0 && l.DecompressedBytes > 0 &&
		l.FileBytes > 0 && l.ManifestBytes > 0 && l.Files > 0
}

// Payload is an upload that has been read and found structurally sound. Whether it may be
// published — by this publisher, under this name, at this version — is decided elsewhere.
type Payload struct {
	Manifest manifest.Manifest
	Files    []string
	Size     int64
	Digest   storage.Digest
}

func Read(content []byte, limits Limits) (Payload, error) {
	if len(content) == 0 {
		return Payload{}, errors.New("the upload is empty")
	}

	if int64(len(content)) > limits.CompressedBytes {
		return Payload{}, fmt.Errorf("the upload is %d bytes, and at most %d may be published at once", len(content), limits.CompressedBytes)
	}

	unzipped, err := gzip.NewReader(bytes.NewReader(content))
	if err != nil {
		return Payload{}, fmt.Errorf("the upload is not a gzipped tarball: %w", err)
	}
	defer unzipped.Close()

	// a second bound under the per-entry ones, so nothing depends on reasoning about what
	// archive/tar will and will not read past a declared size
	archive := tar.NewReader(io.LimitReader(unzipped, limits.DecompressedBytes+1))

	var (
		files      []string
		seen       = map[string]bool{}
		total      int64
		manifested []byte
	)

	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Payload{}, fmt.Errorf("the upload is not a readable tarball: %w", err)
		}

		name, err := safePath(header.Name)
		if err != nil {
			return Payload{}, err
		}

		if header.Typeflag == tar.TypeDir {
			continue
		}

		if header.Typeflag != tar.TypeReg {
			return Payload{}, fmt.Errorf("%q is not a regular file, and a package is source", name)
		}

		if seen[name] {
			return Payload{}, fmt.Errorf("the upload names %q more than once", name)
		}
		seen[name] = true

		if len(files) == limits.Files {
			return Payload{}, fmt.Errorf("the upload holds more than %d files", limits.Files)
		}

		if header.Size > limits.FileBytes {
			return Payload{}, fmt.Errorf("%q is %d bytes, and at most %d may be published in one file", name, header.Size, limits.FileBytes)
		}

		// checked against the declared size before reading, because the declaration is
		// what a bomb inflates: a small upload can claim to expand to gigabytes
		if total += header.Size; total > limits.DecompressedBytes {
			return Payload{}, fmt.Errorf("the upload expands to more than %d bytes", limits.DecompressedBytes)
		}

		if name == manifest.FileName {
			if header.Size > limits.ManifestBytes {
				return Payload{}, fmt.Errorf("%s is %d bytes, and at most %d may describe a package",
					manifest.FileName, header.Size, limits.ManifestBytes)
			}

			manifested, err = io.ReadAll(io.LimitReader(archive, limits.ManifestBytes+1))
			if err != nil {
				return Payload{}, fmt.Errorf("reading %s: %w", manifest.FileName, err)
			}
		}

		files = append(files, name)
	}

	if manifested == nil {
		return Payload{}, fmt.Errorf("the upload has no %s at its root, so there is nothing to publish", manifest.FileName)
	}

	read, err := manifest.Parse(manifested)
	if err != nil {
		return Payload{}, err
	}

	if read.Package == nil {
		return Payload{}, fmt.Errorf("the project has no [package] table, so it has no identity to publish under")
	}

	if !slices.ContainsFunc(files, isLoomSource) {
		return Payload{}, errors.New("the upload holds no .loom files, so there is nothing to publish")
	}

	slices.Sort(files)

	return Payload{
		Manifest: read,
		Files:    files,
		Size:     int64(len(content)),
		Digest:   storage.DigestOf(content),
	}, nil
}

func isLoomSource(name string) bool {
	return strings.EqualFold(path.Ext(name), ".loom")
}

// safePath answers the archive path an extractor would write to, refusing anything that
// could land outside the directory it is extracted into. Backslashes are refused too:
// they are an ordinary character here and a separator on a client's Windows machine.
func safePath(name string) (string, error) {
	switch {
	case name == "":
		return "", errors.New("the upload holds an entry with no name")
	case strings.ContainsRune(name, 0):
		return "", errors.New("the upload holds an entry whose name contains a null byte")
	case strings.ContainsRune(name, '\\'):
		return "", fmt.Errorf("%q contains a backslash, which is a path separator where the package will be installed", name)
	case path.IsAbs(name):
		return "", fmt.Errorf("%q is an absolute path", name)
	}

	cleaned := path.Clean(name)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || path.IsAbs(cleaned) {
		return "", fmt.Errorf("%q resolves outside the package", name)
	}

	return cleaned, nil
}
