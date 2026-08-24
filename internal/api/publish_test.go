package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rbx-loom/loom-pm/internal/auth"
	"github.com/rbx-loom/loom-pm/internal/index"
	"github.com/rbx-loom/loom-pm/internal/manifest"
	"github.com/rbx-loom/loom-pm/internal/pkgname"
	"github.com/rbx-loom/loom-pm/internal/publish"
	"github.com/rbx-loom/loom-pm/internal/semver"
)

type fakeAuth struct {
	tokens map[string]auth.User
	err    error
}

func (f fakeAuth) UserByTokenHash(_ context.Context, hash []byte) (auth.User, error) {
	if f.err != nil {
		return auth.User{}, f.err
	}

	user, ok := f.tokens[string(hash)]
	if !ok {
		return auth.User{}, auth.ErrUnauthenticated
	}

	return user, nil
}

type fakePublishStore struct {
	recorded  []publish.Record
	recordErr error
}

func (f *fakePublishStore) Record(_ context.Context, record publish.Record) error {
	if f.recordErr != nil {
		return f.recordErr
	}

	f.recorded = append(f.recorded, record)
	return nil
}

func (f *fakePublishStore) Unsatisfiable(context.Context, []manifest.Dependency) ([]manifest.Dependency, error) {
	return nil, nil
}

type fakeYanker struct {
	yanked map[string]bool
	err    error
}

func (f *fakeYanker) Yank(_ context.Context, name pkgname.Name, version semver.Version, yanked bool, _ int64) error {
	if f.err != nil {
		return f.err
	}

	if f.yanked == nil {
		f.yanked = map[string]bool{}
	}

	f.yanked[name.String()+"@"+version.String()] = yanked
	return nil
}

func post(t *testing.T, handler http.Handler, target, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestPublishRequiresAToken(t *testing.T) {
	harness := newHarness(t)

	response := post(t, harness.handler, "/v1/publish", "", harness.tarball)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}

	if response.Header().Get("WWW-Authenticate") == "" {
		t.Error("401 carried no WWW-Authenticate header")
	}
}

func TestPublishRejectsAnUnknownToken(t *testing.T) {
	harness := newHarness(t)

	response := post(t, harness.handler, "/v1/publish", "loom_pat_never-issued", harness.tarball)
	if response.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", response.Code)
	}
}

func TestPublishAcceptsAPackage(t *testing.T) {
	harness := newHarness(t)

	response := post(t, harness.handler, "/v1/publish", harness.token, harness.tarball)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", response.Code, response.Body)
	}

	if len(harness.publishStore.recorded) != 1 {
		t.Fatalf("recorded %d publications, want 1", len(harness.publishStore.recorded))
	}

	if got := harness.publishStore.recorded[0].PublisherID; got != 7 {
		t.Errorf("recorded publisher %d, want the authenticated user", got)
	}

	body := response.Body.String()
	for _, want := range []string{"serio", "1.2.0", "sha256:"} {
		if !strings.Contains(body, want) {
			t.Errorf("the response %s does not carry %q", body, want)
		}
	}
}

// A publisher's own mistake is theirs to fix, and must not read as the registry failing.
func TestPublishRejectsABadUpload(t *testing.T) {
	harness := newHarness(t)

	response := post(t, harness.handler, "/v1/publish", harness.token, []byte("not a tarball at all"))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}

	if details := decodeErrors(t, response.Body); len(details) == 0 {
		t.Error("400 carried no diagnostic")
	}
}

func TestPublishRefusals(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"already published", publish.ErrAlreadyPublished, http.StatusConflict},
		{"squatted", publish.ErrSquatted, http.StatusConflict},
		{"not owned", publish.ErrNotOwned, http.StatusForbidden},
		{"not a scope member", publish.ErrNotScopeMember, http.StatusForbidden},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newHarness(t)
			harness.publishStore.recordErr = testCase.err

			response := post(t, harness.handler, "/v1/publish", harness.token, harness.tarball)
			if response.Code != testCase.status {
				t.Errorf("status = %d, want %d", response.Code, testCase.status)
			}

			if details := decodeErrors(t, response.Body); len(details) == 0 {
				t.Error("the refusal carried no diagnostic")
			}
		})
	}
}

func TestPublishRejectsAnOversizedBody(t *testing.T) {
	harness := newHarness(t)

	response := post(t, harness.handler, "/v1/publish", harness.token, bytes.Repeat([]byte("x"), int(harness.limits.CompressedBytes)+1024))
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", response.Code)
	}
}

func TestPublishReportsAStoreFailureAsAFailure(t *testing.T) {
	harness := newHarness(t)
	harness.publishStore.recordErr = errors.New("connection refused")

	response := post(t, harness.handler, "/v1/publish", harness.token, harness.tarball)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.Code)
	}

	if details := decodeErrors(t, response.Body); strings.Contains(strings.Join(details, " "), "connection refused") {
		t.Errorf("the internal error leaked: %v", details)
	}
}

func yank(t *testing.T, handler http.Handler, method, target, token string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(method, target, nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestYankAndUnyank(t *testing.T) {
	harness := newHarness(t)

	response := yank(t, harness.handler, http.MethodPut, "/v1/packages/serio/1.2.0/yank", harness.token)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body)
	}

	if !harness.yanker.yanked["serio@1.2.0"] {
		t.Error("the version was not yanked")
	}

	response = yank(t, harness.handler, http.MethodDelete, "/v1/packages/serio/1.2.0/yank", harness.token)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body)
	}

	if harness.yanker.yanked["serio@1.2.0"] {
		t.Error("the version was not unyanked")
	}
}

func TestYankRequiresAToken(t *testing.T) {
	harness := newHarness(t)

	response := yank(t, harness.handler, http.MethodPut, "/v1/packages/serio/1.2.0/yank", "")
	if response.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", response.Code)
	}
}

func TestYankRefusals(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"unknown version", index.ErrNotFound, http.StatusNotFound},
		{"not owned", publish.ErrNotOwned, http.StatusForbidden},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newHarness(t)
			harness.yanker.err = testCase.err

			response := yank(t, harness.handler, http.MethodPut, "/v1/packages/serio/1.2.0/yank", harness.token)
			if response.Code != testCase.status {
				t.Errorf("status = %d, want %d", response.Code, testCase.status)
			}
		})
	}
}

func TestYankRejectsAnInvalidVersion(t *testing.T) {
	harness := newHarness(t)

	response := yank(t, harness.handler, http.MethodPut, "/v1/packages/serio/not-a-version/yank", harness.token)
	if response.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", response.Code)
	}
}
