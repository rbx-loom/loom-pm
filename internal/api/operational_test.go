package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rbx-loom/loom-pm/internal/auth"
	"github.com/rbx-loom/loom-pm/internal/index"
	"github.com/rbx-loom/loom-pm/internal/pkgname"
	"github.com/rbx-loom/loom-pm/internal/publish"
	"github.com/rbx-loom/loom-pm/internal/storage"
)

type countingStore struct {
	fakeStore

	mu      sync.Mutex
	reads   int
	touches int
}

func (c *countingStore) Package(ctx context.Context, name pkgname.Name) (index.Package, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()

	return c.fakeStore.Package(ctx, name)
}

func (c *countingStore) Modified(ctx context.Context, name pkgname.Name) (time.Time, error) {
	c.mu.Lock()
	c.touches++
	c.mu.Unlock()

	return c.fakeStore.Modified(ctx, name)
}

type recordingUsage struct {
	downloads []int64
	tokens    [][]byte
}

func (r *recordingUsage) Download(versionID int64) { r.downloads = append(r.downloads, versionID) }
func (r *recordingUsage) TokenUsed(hash []byte)    { r.tokens = append(r.tokens, hash) }

func TestHealthIsServedWithoutTheDatabase(t *testing.T) {
	// nothing behind it: a liveness check must not fail for somebody else's outage
	handler := New(Dependencies{Logger: slog.New(slog.DiscardHandler)})

	response := get(t, handler, "/healthz", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}

	if strings.TrimSpace(response.Body.String()) != "ok" {
		t.Errorf("body = %q, want ok", response.Body.String())
	}
}

func TestNewRefusesPartialLimits(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Error("a Limits with only one bound set was accepted")
		}
	}()

	New(Dependencies{Limits: publish.Limits{CompressedBytes: 1 << 20}})
}

// A missing logger degrades, where a missing limit fails fast: the error path must not turn
// a 500 into a panic.
func TestNewToleratesANilLogger(t *testing.T) {
	handler := New(Dependencies{Store: fakeStore{err: errors.New("the database is down")}})

	response := get(t, handler, "/v1/index/serio", nil)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.Code)
	}
}

func TestIndexRevalidatesWithoutRebuildingTheDocument(t *testing.T) {
	store := &countingStore{fakeStore: storeWith(t, "serio")}
	handler, _ := testServer(t, store)

	first := get(t, handler, "/v1/index/serio", nil)
	etag := first.Header().Get("ETag")

	for range 3 {
		revalidated := get(t, handler, "/v1/index/serio", http.Header{"If-None-Match": {etag}})
		if revalidated.Code != http.StatusNotModified {
			t.Fatalf("status = %d, want 304", revalidated.Code)
		}
	}

	if store.reads != 1 {
		t.Errorf("the package was read %d times, want 1: a revalidation should come from the cache", store.reads)
	}

	if store.touches != 4 {
		t.Errorf("Modified was called %d times, want 4", store.touches)
	}
}

// The cache is keyed on when the package changed, so a yank is visible immediately.
func TestIndexRebuildsWhenThePackageChanges(t *testing.T) {
	store := &countingStore{fakeStore: storeWith(t, "serio")}
	handler, _ := testServer(t, store)

	get(t, handler, "/v1/index/serio", nil)
	store.fakeStore.modified = store.fakeStore.modified.Add(time.Minute)
	get(t, handler, "/v1/index/serio", nil)

	if store.reads != 2 {
		t.Errorf("the package was read %d times, want 2", store.reads)
	}
}

func TestDownloadServesARange(t *testing.T) {
	handler, _ := testServer(t, storeWith(t, "serio"))

	response := get(t, handler, "/v1/packages/serio/1.2.0/download",
		http.Header{"Range": {"bytes=2-5"}})
	if response.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", response.Code)
	}

	if got := response.Body.String(); got != tarball[2:6] {
		t.Errorf("body = %q, want %q", got, tarball[2:6])
	}
}

func TestDownloadIsCounted(t *testing.T) {
	store := storeWith(t, "serio")
	store.packages["serio"].Versions[0].ID = 42

	recorder := &recordingUsage{}
	handler := New(Dependencies{
		Store:  store,
		Blobs:  seededBlobs(t),
		Usage:  recorder,
		Logger: slog.New(slog.DiscardHandler),
	})

	if response := get(t, handler, "/v1/packages/serio/1.2.0/download", nil); response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}

	if len(recorder.downloads) != 1 || recorder.downloads[0] != 42 {
		t.Errorf("recorded %v, want [42]", recorder.downloads)
	}
}

func TestAnAuthenticatedRequestRecordsItsToken(t *testing.T) {
	token, hash, err := auth.NewToken()
	if err != nil {
		t.Fatalf("auth.NewToken: %v", err)
	}

	recorder := &recordingUsage{}
	handler := New(Dependencies{
		Store:         storeWith(t, "serio"),
		Blobs:         seededBlobs(t),
		Publisher:     publish.NewService(&fakePublishStore{}, seededBlobs(t), publish.DefaultLimits()),
		Authenticator: auth.New(fakeAuth{tokens: map[string]auth.User{string(hash): {ID: 7, Login: "ada"}}}),
		Yanker:        &fakeYanker{},
		Usage:         recorder,
		Logger:        slog.New(slog.DiscardHandler),
	})

	post(t, handler, "/v1/publish", token, publishableTarball(t))

	if len(recorder.tokens) != 1 {
		t.Fatalf("recorded %d token uses, want 1", len(recorder.tokens))
	}

	if string(recorder.tokens[0]) != string(hash) {
		t.Error("the recorded hash is not the one that was presented")
	}
}

func TestPublishIsRateLimitedPerToken(t *testing.T) {
	harness := newHarness(t)

	for range publishBurst {
		if response := post(t, harness.handler, "/v1/publish", harness.token, harness.tarball); response.Code == http.StatusTooManyRequests {
			t.Fatal("the burst was refused before it was spent")
		}
	}

	response := post(t, harness.handler, "/v1/publish", harness.token, harness.tarball)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", response.Code)
	}

	if response.Header().Get("Retry-After") == "" {
		t.Error("429 carried no Retry-After")
	}
}

// The id is minted here rather than taken from the request, so a client cannot forge or
// collide with an entry in the registry's own logs.
func TestEveryResponseCarriesARequestID(t *testing.T) {
	handler, _ := testServer(t, storeWith(t))

	response := get(t, handler, "/v1/index/serio", http.Header{RequestIDHeader: {"forged"}})

	id := response.Header().Get(RequestIDHeader)
	if id == "" || id == "forged" {
		t.Fatalf("%s = %q, want one minted here", RequestIDHeader, id)
	}

	if !strings.Contains(response.Body.String(), id) {
		t.Error("the failure envelope does not carry the id its log line does")
	}
}

func TestLimiterRefillsOverTime(t *testing.T) {
	limiter := newLimiter(time.Second, 2)
	start := time.Date(2026, 3, 14, 9, 21, 0, 0, time.UTC)

	for range 2 {
		if _, allowed := limiter.allow(1, start); !allowed {
			t.Fatal("the burst was refused before it was spent")
		}
	}

	wait, allowed := limiter.allow(1, start)
	if allowed {
		t.Fatal("a spent bucket allowed another take")
	}
	if wait <= 0 {
		t.Errorf("wait = %s, want a positive one", wait)
	}

	if _, allowed := limiter.allow(1, start.Add(time.Second)); !allowed {
		t.Error("the bucket did not refill")
	}

	// keyed per user: one publisher's burst is not another's
	if _, allowed := limiter.allow(2, start); !allowed {
		t.Error("a second key was refused the first one's allowance")
	}
}

func seededBlobs(t *testing.T) storage.Blobs {
	t.Helper()

	blobs := storage.NewFilesystem(t.TempDir())
	if _, err := blobs.Put(context.Background(), []byte(tarball)); err != nil {
		t.Fatalf("seeding the blob store: %v", err)
	}

	return blobs
}
