// Package maintain holds the jobs a registry needs run against it rather than by it:
// removing blobs nothing references, and checking that what is referenced is still there
// and still itself.
//
// Neither runs on the serving path. Both are commands an operator invokes, which is why
// removing anything is opt-in rather than a default.
package maintain

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/rbx-loom/loom-pm/internal/storage"
)

// Blobs is the blob store as maintenance uses it, which is more than serving needs.
type Blobs interface {
	Walk(ctx context.Context, fn func(digest storage.Digest, size int64, modified time.Time) error) error
	Open(ctx context.Context, digest storage.Digest) (io.ReadCloser, int64, error)
	Delete(ctx context.Context, digest storage.Digest) error
}

// Publication is one published version as maintenance needs to name it.
type Publication struct {
	Name     string
	Version  string
	Checksum storage.Digest
}

func (p Publication) String() string {
	return p.Name + " " + p.Version
}

type Store interface {
	// Checksums answers every checksum a version references. It is materialised rather
	// than streamed because a sweep asks "is this one referenced" per blob; at a million
	// versions that is some tens of megabytes, and a sweep is a command rather than a
	// request.
	Checksums(ctx context.Context) (map[storage.Digest]struct{}, error)

	EachPublication(ctx context.Context, fn func(Publication) error) error
}

type SweepReport struct {
	Scanned  int
	Orphaned int
	Spared   int
	Removed  int
	Bytes    int64
}

// Sweep removes blobs no version references.
//
// A publish writes its blob before its row, so an orphan is what a failure between the two
// leaves behind. That ordering is deliberate — a blob with no row is garbage, a row with no
// blob is a package nobody can install — and this is the other half of it.
//
// An orphan younger than grace is spared, because it is indistinguishable from the blob of
// a publish whose row has not committed yet.
func Sweep(ctx context.Context, blobs Blobs, store Store, grace time.Duration, now time.Time, remove bool) (SweepReport, error) {
	// asked for first, and fatal when it fails: without it every blob looks unreferenced,
	// and a sweep that believed that would empty the store
	live, err := store.Checksums(ctx)
	if err != nil {
		return SweepReport{}, fmt.Errorf("maintain: reading what is referenced: %w", err)
	}

	var report SweepReport

	err = blobs.Walk(ctx, func(digest storage.Digest, size int64, modified time.Time) error {
		report.Scanned++

		if _, referenced := live[digest]; referenced {
			return nil
		}

		report.Orphaned++

		if now.Sub(modified) < grace {
			report.Spared++
			return nil
		}

		if !remove {
			return nil
		}

		if err := blobs.Delete(ctx, digest); err != nil {
			return err
		}

		report.Removed++
		report.Bytes += size
		return nil
	})
	if err != nil {
		return report, fmt.Errorf("maintain: sweeping: %w", err)
	}

	return report, nil
}

type VerifyReport struct {
	Checked int
	Missing []Publication
	Corrupt []Publication
}

// Sound answers whether every published version is still installable.
func (r VerifyReport) Sound() bool {
	return len(r.Missing) == 0 && len(r.Corrupt) == 0
}

// Verify checks that every published version's blob is present and still hashes to the
// checksum recorded for it.
//
// The checksum is what a client verifies a download against, so a blob that no longer
// matches is worse than one that is gone: a missing blob fails loudly, and a changed one
// would be installed. This is what to run after restoring a backup.
func Verify(ctx context.Context, blobs Blobs, store Store) (VerifyReport, error) {
	var report VerifyReport

	err := store.EachPublication(ctx, func(published Publication) error {
		report.Checked++

		content, _, err := blobs.Open(ctx, published.Checksum)
		if errors.Is(err, storage.ErrNotFound) {
			report.Missing = append(report.Missing, published)
			return nil
		}
		if err != nil {
			return err
		}
		defer content.Close()

		hash := sha256.New()
		if _, err := io.Copy(hash, content); err != nil {
			return fmt.Errorf("maintain: reading %s: %w", published, err)
		}

		if storage.Digest(hash.Sum(nil)) != published.Checksum {
			report.Corrupt = append(report.Corrupt, published)
		}

		return nil
	})
	if err != nil {
		return report, fmt.Errorf("maintain: verifying: %w", err)
	}

	return report, nil
}
