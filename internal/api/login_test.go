package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rbx-loom/loom-pm/internal/auth"
)

type fakeProvider struct {
	identity auth.Identity
	err      error
	code     string
}

func (f *fakeProvider) AuthorizeURL(state string) string {
	return "https://github.test/login/oauth/authorize?state=" + url.QueryEscape(state)
}

func (f *fakeProvider) Identify(_ context.Context, code string) (auth.Identity, error) {
	f.code = code
	if f.err != nil {
		return auth.Identity{}, f.err
	}

	return f.identity, nil
}

type fakeUsers struct {
	upserted []auth.Identity
	id       int64
	err      error
}

func (f *fakeUsers) UpsertGitHubUser(_ context.Context, identity auth.Identity) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}

	f.upserted = append(f.upserted, identity)
	return f.id, nil
}

// begin starts a sign-in and answers the state the registry chose along with the cookie it
// expects back.
func (h *harness) begin(t *testing.T) (string, *http.Cookie) {
	t.Helper()

	response := send(t, h.handler, http.MethodGet, "/v1/auth/github", "", "")
	if response.Code != http.StatusFound {
		t.Fatalf("starting sign-in = %d, want 302: %s", response.Code, response.Body)
	}

	location, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatalf("the redirect is not a URL: %v", err)
	}

	state := location.Query().Get("state")
	if state == "" {
		t.Fatal("the redirect carries no state")
	}

	cookies := (&http.Response{Header: response.Header()}).Cookies()
	if len(cookies) != 1 {
		t.Fatalf("the redirect set %d cookies, want the state one", len(cookies))
	}

	return state, cookies[0]
}

func (h *harness) callback(t *testing.T, query string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodGet, "/v1/auth/github/callback?"+query, nil)
	if cookie != nil {
		request.AddCookie(cookie)
	}

	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, request)
	return recorder
}

func TestSignInRedirectsToTheProvider(t *testing.T) {
	harness := newHarness(t)

	_, cookie := harness.begin(t)

	if !cookie.HttpOnly {
		t.Error("the state cookie is readable by scripts")
	}

	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax so the cookie survives the return trip", cookie.SameSite)
	}

	if cookie.Value == "" {
		t.Error("the state cookie carries no value")
	}
}

func TestSignInCompletes(t *testing.T) {
	harness := newHarness(t)
	state, cookie := harness.begin(t)

	response := harness.callback(t, "code=the-code&state="+url.QueryEscape(state), cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("callback = %d, want 200: %s", response.Code, response.Body)
	}

	if harness.provider.code != "the-code" {
		t.Errorf("the provider was given %q, want the-code", harness.provider.code)
	}

	if len(harness.users.upserted) != 1 || harness.users.upserted[0].Login != "ada" {
		t.Fatalf("upserted %+v, want ada", harness.users.upserted)
	}

	body := response.Body.String()
	if !strings.Contains(body, auth.TokenPrefix) {
		t.Errorf("the page does not show a token: %s", body)
	}

	// the page carries a credential, so nothing may keep a copy of it
	if store := response.Header().Get("Cache-Control"); !strings.Contains(store, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", store)
	}

	// the state is spent; leaving it set invites it being replayed
	for _, spent := range (&http.Response{Header: response.Header()}).Cookies() {
		if spent.Name == cookie.Name && spent.MaxAge >= 0 && spent.Value != "" {
			t.Errorf("the state cookie was left set: %+v", spent)
		}
	}
}

// The state is the only thing stopping somebody else's callback being accepted as yours.
func TestSignInRejectsABadState(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		cookied bool
	}{
		{"no cookie", "code=the-code&state=whatever", false},
		{"no state in the query", "code=the-code", true},
		{"a state that does not match", "code=the-code&state=someone-elses", true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newHarness(t)
			_, cookie := harness.begin(t)

			if !testCase.cookied {
				cookie = nil
			}

			response := harness.callback(t, testCase.query, cookie)
			if response.Code != http.StatusBadRequest {
				t.Errorf("callback = %d, want 400", response.Code)
			}

			if len(harness.users.upserted) != 0 {
				t.Error("a user was created from a callback that failed its state check")
			}
		})
	}
}

func TestSignInReportsTheProvidersRefusal(t *testing.T) {
	harness := newHarness(t)
	state, cookie := harness.begin(t)

	response := harness.callback(t, "error=access_denied&error_description=The+user+said+no&state="+url.QueryEscape(state), cookie)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("callback = %d, want 400: %s", response.Code, response.Body)
	}

	if !strings.Contains(response.Body.String(), "said no") {
		t.Errorf("the page does not explain the refusal: %s", response.Body)
	}
}

func TestSignInRequiresACode(t *testing.T) {
	harness := newHarness(t)
	state, cookie := harness.begin(t)

	if response := harness.callback(t, "state="+url.QueryEscape(state), cookie); response.Code != http.StatusBadRequest {
		t.Errorf("callback without a code = %d, want 400", response.Code)
	}
}

// GitHub being unreachable is not the signer-in's fault, and must not read as one.
func TestSignInReportsAProviderFailure(t *testing.T) {
	harness := newHarness(t)
	harness.provider.err = errors.New("connection refused")

	state, cookie := harness.begin(t)

	response := harness.callback(t, "code=the-code&state="+url.QueryEscape(state), cookie)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("callback = %d, want 502: %s", response.Code, response.Body)
	}

	if strings.Contains(response.Body.String(), "connection refused") {
		t.Errorf("the internal error leaked: %s", response.Body)
	}
}

// A self-hosted registry may never configure sign-in, and must say so rather than fail.
func TestSignInUnconfigured(t *testing.T) {
	harness := newHarnessWithout(t)

	for _, path := range []string{"/v1/auth/github", "/v1/auth/github/callback?code=x&state=y"} {
		t.Run(path, func(t *testing.T) {
			response := send(t, harness.handler, http.MethodGet, path, "", "")
			if response.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", response.Code)
			}

			if details := decodeErrors(t, response.Body); len(details) == 0 {
				t.Error("the refusal carried no diagnostic")
			}
		})
	}
}
