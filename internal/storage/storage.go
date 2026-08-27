// Package storage holds package tarballs, addressed by the sha256 of their contents.
//
// The interface is small so the filesystem backend running on the VPS today and an
// object-store backend later differ by a config value rather than an API change.
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

var ErrNotFound = errors.New("storage: no such blob")

const digestPrefix = "sha256:"

// Digest is the sha256 of a blob's contents. Being a fixed-size array rather than a
// string is what makes a stored path unforgeable: it can only ever render as hex, so no
// caller-supplied value can escape the store's root.
type Digest [sha256.Size]byte

func DigestOf(content []byte) Digest {
	return sha256.Sum256(content)
}

func ParseDigest(text string) (Digest, error) {
	encoded, found := strings.CutPrefix(text, digestPrefix)
	if !found {
		return Digest{}, fmt.Errorf("checksum %q is not written %s<hex>", text, digestPrefix)
	}

	decoded, err := hex.DecodeString(encoded)
	if err != nil {
		return Digest{}, fmt.Errorf("checksum %q is not hexadecimal", text)
	}

	if len(decoded) != sha256.Size {
		return Digest{}, fmt.Errorf("checksum %q is %d bytes, want %d", text, len(decoded), sha256.Size)
	}

	return Digest(decoded), nil
}

// String renders the digest the way a lock file records it.
func (d Digest) String() string {
	return digestPrefix + d.Hex()
}

func (d Digest) Hex() string {
	return hex.EncodeToString(d[:])
}

// Blobs stores package tarballs. Put is content-addressed and therefore idempotent:
// storing the same bytes twice is a no-op, which is what lets a failed publish be retried.
//
// There is no Delete: a blob a publish wrote before failing is never referenced and never
// removed. Put takes the whole tarball in memory, which CompressedBytes is what makes
// tolerable; an object-store backend would want a reader and a size instead.
type Blobs interface {
	Put(ctx context.Context, content []byte) (Digest, error)

	// Open answers the blob's contents and its length. The reader also implements
	// io.ReadSeeker whenever the backend can seek, which is what lets a download serve a
	// Range; a backend that cannot must still answer a working ReadCloser.
	Open(ctx context.Context, digest Digest) (io.ReadCloser, int64, error)
}
