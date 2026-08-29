// Package e2e drives the whole registry: real HTTP, real Postgres, real blobs.
//
// Every other package is tested against fakes on the far side of its own interface, which
// proves each half and nothing about the seam between them. This proves that what a
// publisher uploads is what a resolver reads back.
package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rbx-loom/loom-pm/internal/api"
	"github.com/rbx-loom/loom-pm/internal/auth"
	"github.com/rbx-loom/loom-pm/internal/db"
	"github.com/rbx-loom/loom-pm/internal/maintain"
	"github.com/rbx-loom/loom-pm/internal/publish"
	"github.com/rbx-loom/loom-pm/internal/storage"
)

// schema keeps this package's tables away from internal/db's, because go test runs the two
// packages at once and both reset what they use.
const schema = "e2e"

type registry struct {
	url    string
	token  string
	client *http.Client

	store    *db.Store
	blobs    *storage.Filesystem
	blobRoot string
}

// logTo sends the server's log to the test, so a swallowed 500 explains itself in the
// output of the test that provoked it instead of nowhere.
type logTo struct{ t *testing.T }

func (l logTo) Write(line []byte) (int, error) {
	l.t.Logf("server: %s", bytes.TrimRight(line, "\n"))
	return len(line), nil
}

func start(t *testing.T) *registry {
	t.Helper()

	url := os.Getenv("LOOM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set LOOM_TEST_DATABASE_URL to run the end-to-end tests")
	}

	separator := "?"
	if strings.Contains(url, "?") {
		separator = "&"
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url+separator+"search_path="+schema)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE; CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("resetting the schema: %v", err)
	}

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	store := db.NewStore(pool)
	blobRoot := t.TempDir()
	blobs := storage.NewFilesystem(blobRoot)
	limits := publish.DefaultLimits()

	handler := api.New(api.Dependencies{
		Store:         store,
		Blobs:         blobs,
		Publisher:     publish.NewService(store, blobs, limits),
		Authenticator: auth.New(store),
		Yanker:        store,
		Owners:        store,
		Tokens:        store,
		Limits:        limits,
		Logger:        slog.New(slog.NewTextHandler(logTo{t}, nil)),
	})

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	token, err := store.IssueToken(ctx, "ada", "e2e")
	if err != nil {
		t.Fatalf("issuing a token: %v", err)
	}

	return &registry{
		url: server.URL, token: token, client: server.Client(),
		store: store, blobs: blobs, blobRoot: blobRoot,
	}
}

// tarball builds what `loom publish` would send: a manifest at the root and a source file.
func tarball(t *testing.T, name, version string, dependencies map[string]string) []byte {
	t.Helper()

	manifest := fmt.Sprintf("[package]\nname = %q\nversion = %q\n", name, version)
	if len(dependencies) > 0 {
		manifest += "\n[dependencies]\n"
		for depended, requirement := range dependencies {
			manifest += fmt.Sprintf("%q = %q\n", depended, requirement)
		}
	}

	var compressed bytes.Buffer
	zipper := gzip.NewWriter(&compressed)
	writer := tar.NewWriter(zipper)

	files := []struct{ name, content string }{
		{"loom-config.toml", manifest},
		{"src/init.loom", fmt.Sprintf("export fn %s() {}", "main")},
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

func (r *registry) do(t *testing.T, method, path string, body []byte, authenticated bool) (int, []byte) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	request, err := http.NewRequest(method, r.url+path, reader)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}

	if authenticated {
		request.Header.Set("Authorization", "Bearer "+r.token)
	}

	response, err := r.client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()

	read, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}

	return response.StatusCode, read
}

func (r *registry) publish(t *testing.T, name, version string, dependencies map[string]string) (int, []byte) {
	t.Helper()

	return r.do(t, http.MethodPost, "/v1/publish", tarball(t, name, version, dependencies), true)
}

type indexDocument struct {
	Name     string `json:"name"`
	Versions []struct {
		Version      string `json:"version"`
		Checksum     string `json:"checksum"`
		Yanked       bool   `json:"yanked"`
		Dependencies []struct {
			Name        string `json:"name"`
			Requirement string `json:"requirement"`
			Dev         bool   `json:"dev"`
		} `json:"dependencies"`
	} `json:"versions"`
}

func (r *registry) index(t *testing.T, name string) indexDocument {
	t.Helper()

	status, body := r.do(t, http.MethodGet, "/v1/index/"+name, nil, false)
	if status != http.StatusOK {
		t.Fatalf("GET index %s = %d: %s", name, status, body)
	}

	var document indexDocument
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("decoding the index: %v", err)
	}

	return document
}

// A version published is a version resolvable, downloadable, and unchanged in transit.
func TestPublishThenResolveAndDownload(t *testing.T) {
	registry := start(t)

	uploaded := tarball(t, "serio", "1.2.0", map[string]string{"math": "^1.0"})

	// math has to exist first, or the dependency is unsatisfiable and the publish refused
	if status, body := registry.publish(t, "math", "1.0.0", nil); status != http.StatusCreated {
		t.Fatalf("publishing math = %d: %s", status, body)
	}

	status, body := registry.do(t, http.MethodPost, "/v1/publish", uploaded, true)
	if status != http.StatusCreated {
		t.Fatalf("publishing serio = %d: %s", status, body)
	}

	document := registry.index(t, "serio")
	if document.Name != "serio" || len(document.Versions) != 1 {
		t.Fatalf("the index reads %+v, want one version of serio", document)
	}

	published := document.Versions[0]
	if published.Version != "1.2.0" {
		t.Errorf("version = %q, want 1.2.0", published.Version)
	}

	// the checksum a lock file records has to be the checksum of the bytes uploaded, or
	// the client verifies against something the registry invented
	sum := sha256.Sum256(uploaded)
	if want := "sha256:" + hex.EncodeToString(sum[:]); published.Checksum != want {
		t.Errorf("checksum = %q, want %q", published.Checksum, want)
	}

	if len(published.Dependencies) != 1 ||
		published.Dependencies[0].Name != "math" ||
		published.Dependencies[0].Requirement != "^1.0" {
		t.Errorf("dependencies = %+v, want math ^1.0 as written", published.Dependencies)
	}

	status, downloaded := registry.do(t, http.MethodGet, "/v1/packages/serio/1.2.0/download", nil, false)
	if status != http.StatusOK {
		t.Fatalf("downloading = %d: %s", status, downloaded)
	}

	if !bytes.Equal(downloaded, uploaded) {
		t.Errorf("downloaded %d bytes, want the %d uploaded", len(downloaded), len(uploaded))
	}
}

// LockResolver.Choose takes the LAST satisfying entry, so this is the ordering the whole
// stack has to preserve — through Postgres, which returns rows in whatever order it likes.
func TestIndexOrdersVersionsThroughTheDatabase(t *testing.T) {
	registry := start(t)

	for _, version := range []string{"1.10.0", "1.0.0", "2.0.0-alpha", "1.2.0"} {
		if status, body := registry.publish(t, "serio", version, nil); status != http.StatusCreated {
			t.Fatalf("publishing %s = %d: %s", version, status, body)
		}
	}

	document := registry.index(t, "serio")

	want := []string{"1.0.0", "1.2.0", "1.10.0", "2.0.0-alpha"}
	if len(document.Versions) != len(want) {
		t.Fatalf("the index holds %d versions, want %d", len(document.Versions), len(want))
	}

	for index, expected := range want {
		if got := document.Versions[index].Version; got != expected {
			t.Errorf("versions[%d] = %q, want %q", index, got, expected)
		}
	}
}

// A published version is never replaced, and semver identity excludes build metadata, so
// neither spelling gets a second chance at 1.0.0.
func TestPublishRefusesAVersionTwice(t *testing.T) {
	registry := start(t)

	if status, body := registry.publish(t, "serio", "1.0.0", nil); status != http.StatusCreated {
		t.Fatalf("publishing = %d: %s", status, body)
	}

	status, body := registry.publish(t, "serio", "1.0.0", nil)
	if status != http.StatusConflict {
		t.Errorf("republishing = %d, want 409: %s", status, body)
	}

	status, body = registry.publish(t, "serio", "1.0.0+other", nil)
	if status != http.StatusConflict {
		t.Errorf("republishing with build metadata = %d, want 409: %s", status, body)
	}
}

// Yanking excludes a version from a new resolution and from nothing else: a lock file
// already pinning it has to keep installing.
func TestYankedVersionStaysDownloadable(t *testing.T) {
	registry := start(t)

	if status, body := registry.publish(t, "serio", "1.0.0", nil); status != http.StatusCreated {
		t.Fatalf("publishing = %d: %s", status, body)
	}

	if status, body := registry.do(t, http.MethodPut, "/v1/packages/serio/1.0.0/yank", nil, true); status != http.StatusOK {
		t.Fatalf("yanking = %d: %s", status, body)
	}

	document := registry.index(t, "serio")
	if len(document.Versions) != 1 || !document.Versions[0].Yanked {
		t.Fatalf("the index reads %+v, want the version present and yanked", document.Versions)
	}

	if status, _ := registry.do(t, http.MethodGet, "/v1/packages/serio/1.0.0/download", nil, false); status != http.StatusOK {
		t.Errorf("downloading a yanked version = %d, want 200", status)
	}

	if status, body := registry.do(t, http.MethodDelete, "/v1/packages/serio/1.0.0/yank", nil, true); status != http.StatusOK {
		t.Fatalf("unyanking = %d: %s", status, body)
	}

	if document := registry.index(t, "serio"); document.Versions[0].Yanked {
		t.Error("the version is still yanked after being unyanked")
	}
}

// A version whose dependency no published version satisfies could never be resolved, so
// publishing it would only hand somebody a broken package.
func TestPublishRefusesAnUnresolvableDependency(t *testing.T) {
	registry := start(t)

	status, body := registry.publish(t, "app", "1.0.0", map[string]string{"nowhere": "^1.0"})
	if status != http.StatusBadRequest {
		t.Fatalf("publishing = %d, want 400: %s", status, body)
	}

	if !strings.Contains(string(body), "nowhere") {
		t.Errorf("the diagnostic %s does not name the dependency", body)
	}

	if status, _ := registry.do(t, http.MethodGet, "/v1/index/app", nil, false); status != http.StatusNotFound {
		t.Errorf("the refused package is in the index: %d", status)
	}
}

// Publishing creates the package and makes the publisher its first owner, which is what
// every later publish and yank checks against.
func TestFirstPublishTakesOwnership(t *testing.T) {
	registry := start(t)

	if status, body := registry.publish(t, "serio", "1.0.0", nil); status != http.StatusCreated {
		t.Fatalf("publishing = %d: %s", status, body)
	}

	status, body := registry.do(t, http.MethodGet, "/v1/packages/serio/owners", nil, false)
	if status != http.StatusOK {
		t.Fatalf("reading owners = %d: %s", status, body)
	}

	var owners struct {
		Owners []string `json:"owners"`
	}
	if err := json.Unmarshal(body, &owners); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	if strings.Join(owners.Owners, ",") != "ada" {
		t.Errorf("owners = %v, want the publisher", owners.Owners)
	}
}

// The index is revalidated far more often than it changes, so the ETag has to survive a
// round trip through the database and the cache.
func TestIndexRevalidatesEndToEnd(t *testing.T) {
	registry := start(t)

	if status, body := registry.publish(t, "serio", "1.0.0", nil); status != http.StatusCreated {
		t.Fatalf("publishing = %d: %s", status, body)
	}

	request, err := http.NewRequest(http.MethodGet, registry.url+"/v1/index/serio", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}

	first, err := registry.client.Do(request)
	if err != nil {
		t.Fatalf("GET index: %v", err)
	}
	first.Body.Close()

	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("the index carried no ETag")
	}

	request.Header.Set("If-None-Match", etag)
	second, err := registry.client.Do(request)
	if err != nil {
		t.Fatalf("revalidating: %v", err)
	}
	second.Body.Close()

	if second.StatusCode != http.StatusNotModified {
		t.Errorf("revalidating = %d, want 304", second.StatusCode)
	}

	// publishing changes the document, so the tag it was revalidating against must stop
	// matching or a client caches a version list missing its newest entry
	if status, body := registry.publish(t, "serio", "1.1.0", nil); status != http.StatusCreated {
		t.Fatalf("publishing again = %d: %s", status, body)
	}

	third, err := registry.client.Do(request)
	if err != nil {
		t.Fatalf("revalidating after a publish: %v", err)
	}
	third.Body.Close()

	if third.StatusCode != http.StatusOK {
		t.Errorf("revalidating after a publish = %d, want 200", third.StatusCode)
	}
}

// pathOf is where the filesystem store keeps a blob, which a corruption test has to reach
// past the store to change.
func (r *registry) pathOf(digest storage.Digest) string {
	hex := digest.Hex()
	return filepath.Join(r.blobRoot, hex[0:2], hex[2:4], hex)
}

// Verify is what an operator runs after restoring a backup, so it has to agree with what
// publishing actually wrote.
func TestVerifyIsSoundAfterPublishing(t *testing.T) {
	registry := start(t)

	for _, version := range []string{"1.0.0", "1.1.0"} {
		if status, body := registry.publish(t, "serio", version, nil); status != http.StatusCreated {
			t.Fatalf("publishing %s = %d: %s", version, status, body)
		}
	}

	report, err := maintain.Verify(context.Background(), registry.blobs, registry.store)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if report.Checked != 2 || !report.Sound() {
		t.Errorf("report = %+v, want both versions checked and sound", report)
	}
}

// A blob that no longer hashes to its recorded checksum would be installed by a client
// that trusts the index, so verifying has to catch it.
func TestVerifyCatchesCorruption(t *testing.T) {
	registry := start(t)

	uploaded := tarball(t, "serio", "1.0.0", nil)
	if status, body := registry.do(t, http.MethodPost, "/v1/publish", uploaded, true); status != http.StatusCreated {
		t.Fatalf("publishing = %d: %s", status, body)
	}

	if err := os.WriteFile(registry.pathOf(storage.DigestOf(uploaded)), []byte("not what was published"), 0o644); err != nil {
		t.Fatalf("corrupting the blob: %v", err)
	}

	report, err := maintain.Verify(context.Background(), registry.blobs, registry.store)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if len(report.Corrupt) != 1 || report.Corrupt[0].Name != "serio" {
		t.Fatalf("report = %+v, want serio reported corrupt", report)
	}

	if report.Sound() {
		t.Error("a corrupt store reported itself sound")
	}
}

// Sweeping must never touch a blob a version references, however old it is.
func TestSweepSparesPublishedBlobs(t *testing.T) {
	registry := start(t)

	uploaded := tarball(t, "serio", "1.0.0", nil)
	if status, body := registry.do(t, http.MethodPost, "/v1/publish", uploaded, true); status != http.StatusCreated {
		t.Fatalf("publishing = %d: %s", status, body)
	}

	published := storage.DigestOf(uploaded)

	// old enough that only being referenced can save it
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(registry.pathOf(published), old, old); err != nil {
		t.Fatalf("backdating the blob: %v", err)
	}

	orphan, err := registry.blobs.Put(context.Background(), []byte("left by a publish that failed"))
	if err != nil {
		t.Fatalf("planting an orphan: %v", err)
	}
	if err := os.Chtimes(registry.pathOf(orphan), old, old); err != nil {
		t.Fatalf("backdating the orphan: %v", err)
	}

	report, err := maintain.Sweep(context.Background(), registry.blobs, registry.store, time.Hour, time.Now(), true)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if report.Removed != 1 {
		t.Fatalf("report = %+v, want only the orphan removed", report)
	}

	if _, _, err := registry.blobs.Open(context.Background(), published); err != nil {
		t.Errorf("the published blob was swept: %v", err)
	}

	// and the package is still downloadable, which is the thing that actually matters
	if status, _ := registry.do(t, http.MethodGet, "/v1/packages/serio/1.0.0/download", nil, false); status != http.StatusOK {
		t.Errorf("downloading after a sweep = %d, want 200", status)
	}
}
