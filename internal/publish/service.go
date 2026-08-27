package publish

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rbx-loom/loom-pm/internal/manifest"
	"github.com/rbx-loom/loom-pm/internal/storage"
)

var (
	ErrAlreadyPublished = errors.New("publish: that version is already published")
	ErrNotOwned         = errors.New("publish: the package belongs to someone else")
	ErrSquatted         = errors.New("publish: the name is too close to one already taken")
	ErrNotScopeMember   = errors.New("publish: not a member of that scope")
)

// InvalidUpload marks a failure the publisher caused and can fix, as against one the
// registry is having. The two are the difference between a 400 and a 500.
type InvalidUpload struct{ Err error }

func (e *InvalidUpload) Error() string { return e.Err.Error() }
func (e *InvalidUpload) Unwrap() error { return e.Err }

type Record struct {
	Payload     Payload
	PublisherID int64
}

type Store interface {
	// Record writes the version, creating the package and its first owner when the name is
	// free. It answers ErrAlreadyPublished, ErrNotOwned, ErrSquatted or ErrNotScopeMember
	// rather than writing something the caller did not ask for.
	Record(ctx context.Context, record Record) error

	// Unsatisfiable answers the dependencies that no published version satisfies.
	Unsatisfiable(ctx context.Context, dependencies []manifest.Dependency) ([]manifest.Dependency, error)
}

type Service struct {
	store  Store
	blobs  storage.Blobs
	limits Limits
}

func NewService(store Store, blobs storage.Blobs, limits Limits) *Service {
	return &Service{store: store, blobs: blobs, limits: limits}
}

func (s *Service) Publish(ctx context.Context, content []byte, publisherID int64) (Payload, error) {
	payload, err := Read(content, s.limits)
	if err != nil {
		return Payload{}, &InvalidUpload{Err: err}
	}

	if err := s.resolvable(ctx, payload.Manifest.Dependencies); err != nil {
		return Payload{}, err
	}

	// before the row, so a failure between the two leaves an unreferenced blob rather than
	// a version nobody can install. Nothing sweeps those: they are permanent, and bounded
	// by CompressedBytes per failed publish, which is the cheaper of the two leaks.
	if _, err := s.blobs.Put(ctx, content); err != nil {
		return Payload{}, err
	}

	if err := s.store.Record(ctx, Record{Payload: payload, PublisherID: publisherID}); err != nil {
		return Payload{}, err
	}

	return payload, nil
}

// resolvable refuses a version depending on something that can never resolve. Development
// dependencies are excluded: they are what a package's own tests are written against and
// no part of compiling it for anyone else, which is the same line PublishedPackage draws.
func (s *Service) resolvable(ctx context.Context, dependencies []manifest.Dependency) error {
	required := make([]manifest.Dependency, 0, len(dependencies))
	for _, dependency := range dependencies {
		if !dependency.Dev {
			required = append(required, dependency)
		}
	}

	if len(required) == 0 {
		return nil
	}

	missing, err := s.store.Unsatisfiable(ctx, required)
	if err != nil {
		return err
	}

	if len(missing) == 0 {
		return nil
	}

	described := make([]string, 0, len(missing))
	for _, dependency := range missing {
		described = append(described, fmt.Sprintf("'%s' %s", dependency.Name, dependency.Requirement))
	}

	return &InvalidUpload{Err: fmt.Errorf(
		"no published version satisfies %s, so this version could never be resolved", strings.Join(described, ", "))}
}
