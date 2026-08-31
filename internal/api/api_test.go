package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rbx-loom/loom-pm/internal/auth"
	"github.com/rbx-loom/loom-pm/internal/index"
	"github.com/rbx-loom/loom-pm/internal/pkgname"
	"github.com/rbx-loom/loom-pm/internal/publish"
	"github.com/rbx-loom/loom-pm/internal/semver"
	"github.com/rbx-loom/loom-pm/internal/storage"
)

type fakeStore struct {
	packages map[string]index.Package
	modified time.Time
	err      error
}

func (f fakeStore) Package(_ context.Context, name pkgname.Name) (index.Package, error) {
	if f.err != nil {
		return index.Package{}, f.err
	}

	pkg, ok := f.packages[name.Normalized()]
	if !ok {
		return index.Package{}, index.ErrNotFound
	}

	return pkg, nil
}

func (f fakeStore) Modified(ctx context.Context, name pkgname.Name) (time.Time, error) {
	if _, err := f.Package(ctx, name); err != nil {
		return time.Time{}, err
	}

	return f.modified, nil
}

func (f fakeStore) Version(ctx context.Context, name pkgname.Name, version semver.Version) (index.Version, error) {
	pkg, err := f.Package(ctx, name)
	if err != nil {
		return index.Version{}, err
	}

	for _, candidate := range pkg.Versions {
		if candidate.Version.Equal(version) {
			return candidate, nil
		}
	}

	return index.Version{}, index.ErrNotFound
}

const tarball = "a gzipped tarball, for the purposes of this test"

func testServer(t *testing.T, store index.Store) (http.Handler, storage.Digest) {
	t.Helper()

	blobs := storage.NewFilesystem(t.TempDir())
	digest, err := blobs.Put(context.Background(), []byte(tarball))
	if err != nil {
		t.Fatalf("seeding the blob store: %v", err)
	}

	return New(Dependencies{
		Store:         store,
		Blobs:         blobs,
		Publisher:     publish.NewService(&fakePublishStore{}, blobs, publish.DefaultLimits()),
		Authenticator: auth.New(fakeAuth{}),
		Yanker:        &fakeYanker{},
		Owners:        &fakeOwners{owners: map[string][]string{}},
		Tokens:        &fakeTokens{},
		Logger:        slog.New(slog.DiscardHandler),
	}), digest
}

// harness is a server with a publisher who holds a live token, and a tarball they may
// publish. The read-path tests above need none of it.
type harness struct {
	handler      http.Handler
	token        string
	tarball      []byte
	limits       publish.Limits
	publishStore *fakePublishStore
	yanker       *fakeYanker
	owners       *fakeOwners
	tokens       *fakeTokens
	provider     *fakeProvider
	users        *fakeUsers
	sessions     *fakeSessions
	catalog      *fakeCatalog
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	token, hash, err := auth.NewToken()
	if err != nil {
		t.Fatalf("auth.NewToken: %v", err)
	}

	blobs := storage.NewFilesystem(t.TempDir())
	store := &fakePublishStore{}
	yanker := &fakeYanker{}
	owners := &fakeOwners{owners: map[string][]string{"serio": {"ada"}}}
	tokens := &fakeTokens{}
	provider := &fakeProvider{identity: auth.Identity{GitHubID: 4242, Login: "ada"}}
	users := &fakeUsers{id: 7}
	sessions := newFakeSessions()
	browsing := catalogWith(t, "serio")
	limits := publish.DefaultLimits()

	return &harness{
		handler: New(Dependencies{
			Store:         storeWith(t, "serio"),
			Blobs:         blobs,
			Publisher:     publish.NewService(store, blobs, limits),
			Authenticator: auth.New(fakeAuth{tokens: map[string]auth.User{string(hash): {ID: 7, Login: "ada"}}}),
			Yanker:        yanker,
			Owners:        owners,
			Tokens:        tokens,
			Provider:      provider,
			Users:         users,
			Sessions:      sessions,
			Catalog:       browsing,
			Limits:        limits,
			Logger:        slog.New(slog.DiscardHandler),
		}),
		token:        token,
		tarball:      publishableTarball(t),
		limits:       limits,
		publishStore: store,
		yanker:       yanker,
		owners:       owners,
		tokens:       tokens,
		provider:     provider,
		users:        users,
		sessions:     sessions,
		catalog:      browsing,
	}
}

// newHarnessWithMetricsToken is a registry whose operator put /metrics behind a token.
func newHarnessWithMetricsToken(t *testing.T, token string) *harness {
	t.Helper()

	built := newHarness(t)
	blobs := storage.NewFilesystem(t.TempDir())

	built.handler = New(Dependencies{
		Store:         storeWith(t, "serio"),
		Blobs:         blobs,
		Publisher:     publish.NewService(&fakePublishStore{}, blobs, publish.DefaultLimits()),
		Authenticator: auth.New(fakeAuth{}),
		Yanker:        &fakeYanker{},
		Owners:        &fakeOwners{owners: map[string][]string{}},
		Tokens:        &fakeTokens{},
		MetricsToken:  token,
		Logger:        slog.New(slog.DiscardHandler),
	})

	return built
}

// newHarnessWithout is a registry whose operator never configured sign-in, which is every
// self-hosted one that mints its tokens from the command line.
func newHarnessWithout(t *testing.T) *harness {
	t.Helper()

	built := newHarness(t)
	built.handler = New(Dependencies{
		Store:         storeWith(t, "serio"),
		Blobs:         storage.NewFilesystem(t.TempDir()),
		Publisher:     publish.NewService(&fakePublishStore{}, storage.NewFilesystem(t.TempDir()), publish.DefaultLimits()),
		Authenticator: auth.New(fakeAuth{}),
		Yanker:        &fakeYanker{},
		Owners:        &fakeOwners{owners: map[string][]string{}},
		Tokens:        &fakeTokens{},
		Logger:        slog.New(slog.DiscardHandler),
	})

	return built
}

func publishableTarball(t *testing.T) []byte {
	t.Helper()

	var compressed bytes.Buffer
	zipper := gzip.NewWriter(&compressed)
	writer := tar.NewWriter(zipper)

	files := []struct{ name, content string }{
		{"loom-config.toml", "[package]\nname = \"serio\"\nversion = \"1.2.0\"\n"},
		{"src/init.loom", "export fn main() {}"},
	}

	for _, written := range files {
		header := &tar.Header{Name: written.name, Mode: 0o644, Typeflag: tar.TypeReg, Size: int64(len(written.content))}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatalf("writing %q: %v", written.name, err)
		}
		if _, err := writer.Write([]byte(written.content)); err != nil {
			t.Fatalf("writing %q: %v", written.name, err)
		}
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("closing the tar: %v", err)
	}
	if err := zipper.Close(); err != nil {
		t.Fatalf("closing the gzip: %v", err)
	}

	return compressed.Bytes()
}

func storeWith(t *testing.T, names ...string) fakeStore {
	t.Helper()

	packages := make(map[string]index.Package, len(names))
	for _, text := range names {
		name, err := pkgname.Parse(text)
		if err != nil {
			t.Fatalf("pkgname.Parse(%q): %v", text, err)
		}

		version, err := semver.ParseVersion("1.2.0")
		if err != nil {
			t.Fatalf("semver.ParseVersion: %v", err)
		}

		packages[name.Normalized()] = index.Package{
			Name: name,
			Versions: []index.Version{{
				Version:     version,
				Checksum:    storage.DigestOf([]byte(tarball)),
				Realm:       index.RealmShared,
				PublishedAt: time.Date(2026, 3, 14, 9, 21, 0, 0, time.UTC),
			}},
		}
	}

	return fakeStore{packages: packages, modified: time.Date(2026, 3, 14, 9, 21, 0, 0, time.UTC)}
}

func get(t *testing.T, handler http.Handler, target string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodGet, target, nil)
	for key, values := range header {
		request.Header[key] = values
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeErrors(t *testing.T, body io.Reader) []string {
	t.Helper()

	var envelope struct {
		Errors []struct {
			Detail string `json:"detail"`
		} `json:"errors"`
	}

	if err := json.NewDecoder(body).Decode(&envelope); err != nil {
		t.Fatalf("the error response is not the documented envelope: %v", err)
	}

	details := make([]string, 0, len(envelope.Errors))
	for _, reported := range envelope.Errors {
		details = append(details, reported.Detail)
	}

	return details
}

func TestIndexServesAPackage(t *testing.T) {
	handler, _ := testServer(t, storeWith(t, "serio"))

	response := get(t, handler, "/v1/index/serio", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}

	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	if response.Header().Get("ETag") == "" {
		t.Error("no ETag, so the document cannot be revalidated")
	}

	if got := response.Header().Get("Cache-Control"); !strings.Contains(got, "max-age") {
		t.Errorf("Cache-Control = %q, want a max-age", got)
	}

	var document struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatalf("decoding the document: %v", err)
	}

	if document.Name != "serio" {
		t.Errorf("name = %q, want serio", document.Name)
	}
}

func TestIndexServesAScopedPackage(t *testing.T) {
	handler, _ := testServer(t, storeWith(t, "scope/serio"))

	response := get(t, handler, "/v1/index/scope/serio", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}

	var document struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatalf("decoding the document: %v", err)
	}

	if document.Name != "scope/serio" {
		t.Errorf("name = %q, want scope/serio", document.Name)
	}
}

// Lookup is case-insensitive, because a package name is compared that way everywhere else.
func TestIndexIsCaseInsensitive(t *testing.T) {
	handler, _ := testServer(t, storeWith(t, "serio"))

	if response := get(t, handler, "/v1/index/SERIO", nil); response.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", response.Code)
	}
}

func TestIndexRevalidates(t *testing.T) {
	handler, _ := testServer(t, storeWith(t, "serio"))

	first := get(t, handler, "/v1/index/serio", nil)
	etag := first.Header().Get("ETag")

	revalidated := get(t, handler, "/v1/index/serio", http.Header{"If-None-Match": {etag}})
	if revalidated.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", revalidated.Code)
	}

	if revalidated.Body.Len() != 0 {
		t.Errorf("304 carried a %d byte body", revalidated.Body.Len())
	}

	if got := revalidated.Header().Get("ETag"); got != etag {
		t.Errorf("304 ETag = %q, want %q", got, etag)
	}
}

func TestIndexServesWhenTheETagDiffers(t *testing.T) {
	handler, _ := testServer(t, storeWith(t, "serio"))

	response := get(t, handler, "/v1/index/serio", http.Header{"If-None-Match": {`"stale"`}})
	if response.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", response.Code)
	}
}

// A package the registry does not publish is a resolution failure for the client to
// report, and must be distinguishable from the registry being unreachable.
func TestIndexNotFound(t *testing.T) {
	handler, _ := testServer(t, storeWith(t))

	response := get(t, handler, "/v1/index/serio", nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}

	if details := decodeErrors(t, response.Body); len(details) == 0 {
		t.Error("404 carried no diagnostic")
	}
}

func TestIndexRejectsAnInvalidName(t *testing.T) {
	handler, _ := testServer(t, storeWith(t))

	response := get(t, handler, "/v1/index/1serio", nil)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}

	details := decodeErrors(t, response.Body)
	if len(details) == 0 || !strings.Contains(details[0], "letter") {
		t.Errorf("diagnostic = %v, want it to explain the name rule", details)
	}
}

// A store failure must not read as "not published": the client would report a missing
// package to a user whose registry was merely unreachable.
func TestIndexReportsAStoreFailureAsAFailure(t *testing.T) {
	handler, _ := testServer(t, fakeStore{err: errors.New("connection refused")})

	response := get(t, handler, "/v1/index/serio", nil)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.Code)
	}

	if details := decodeErrors(t, response.Body); strings.Contains(strings.Join(details, " "), "connection refused") {
		t.Errorf("the internal error leaked to the client: %v", details)
	}
}

func TestDownloadServesTheTarball(t *testing.T) {
	handler, digest := testServer(t, storeWith(t, "serio"))

	response := get(t, handler, "/v1/packages/serio/1.2.0/download", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}

	if response.Body.String() != tarball {
		t.Errorf("body = %q, want the tarball", response.Body.String())
	}

	if got := response.Header().Get("Content-Type"); got != "application/gzip" {
		t.Errorf("Content-Type = %q, want application/gzip", got)
	}

	// a published version is never replaced, so its bytes can be cached forever
	if got := response.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("Cache-Control = %q, want it immutable", got)
	}

	if got := response.Header().Get("ETag"); !strings.Contains(got, digest.Hex()) {
		t.Errorf("ETag = %q, want it to carry the checksum", got)
	}
}

func TestDownloadServesAScopedPackage(t *testing.T) {
	handler, _ := testServer(t, storeWith(t, "scope/serio"))

	if response := get(t, handler, "/v1/packages/scope/serio/1.2.0/download", nil); response.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", response.Code)
	}
}

func TestDownloadNotFound(t *testing.T) {
	handler, _ := testServer(t, storeWith(t, "serio"))

	if response := get(t, handler, "/v1/packages/serio/9.9.9/download", nil); response.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", response.Code)
	}
}

func TestDownloadRejectsAnInvalidVersion(t *testing.T) {
	handler, _ := testServer(t, storeWith(t, "serio"))

	response := get(t, handler, "/v1/packages/serio/not-a-version/download", nil)
	if response.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", response.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	handler, _ := testServer(t, storeWith(t, "serio"))

	request := httptest.NewRequest(http.MethodDelete, "/v1/index/serio", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", recorder.Code)
	}
}

func TestUnknownRoute(t *testing.T) {
	handler, _ := testServer(t, storeWith(t, "serio"))

	if response := get(t, handler, "/v1/nothing-here", nil); response.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", response.Code)
	}
}
