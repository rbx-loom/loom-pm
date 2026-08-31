package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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

// fakeSessions is the session store, in a map. Expiry is modelled as a deadline rather
// than a duration so that a test can age a session without sleeping.
type fakeSessions struct {
	live map[string]sessionRow
	now  time.Time
	err  error
}

type sessionRow struct {
	userID  int64
	expires time.Time
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{live: map[string]sessionRow{}, now: time.Date(2026, 3, 14, 9, 21, 0, 0, time.UTC)}
}

func (f *fakeSessions) CreateSession(_ context.Context, userID int64, lifetime time.Duration) (string, error) {
	if f.err != nil {
		return "", f.err
	}

	presented, hash, err := auth.NewSession()
	if err != nil {
		return "", err
	}

	f.live[string(hash)] = sessionRow{userID: userID, expires: f.now.Add(lifetime)}
	return presented, nil
}

func (f *fakeSessions) UserBySessionHash(_ context.Context, hash []byte) (auth.User, error) {
	if f.err != nil {
		return auth.User{}, f.err
	}

	row, ok := f.live[string(hash)]
	if !ok || !row.expires.After(f.now) {
		return auth.User{}, auth.ErrNoSession
	}

	return auth.User{ID: row.userID, Login: "ada"}, nil
}

func (f *fakeSessions) RevokeSession(_ context.Context, hash []byte) error {
	if f.err != nil {
		return f.err
	}

	delete(f.live, string(hash))
	return nil
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

// signIn drives a whole sign-in and answers the session cookie the registry set, which is
// what the endpoints a token may not reach ask for.
func (h *harness) signIn(t *testing.T) *http.Cookie {
	t.Helper()

	state, started := h.begin(t)
	response := h.callback(t, "code=the-code&state="+url.QueryEscape(state), started)
	if response.Code != http.StatusOK {
		t.Fatalf("signing in = %d, want 200: %s", response.Code, response.Body)
	}

	for _, cookie := range (&http.Response{Header: response.Header()}).Cookies() {
		if cookie.Name == "loom_session" && cookie.Value != "" {
			return cookie
		}
	}

	t.Fatal("the sign-in set no session cookie")
	return nil
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

// A sign-in has to leave the browser holding something, or the token it just showed is the
// last one that browser will ever be able to make.
func TestSignInSetsASession(t *testing.T) {
	harness := newHarness(t)

	session := harness.signIn(t)

	if !session.HttpOnly {
		t.Error("the session cookie is readable by scripts")
	}

	if session.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax so a cross-site POST does not carry it", session.SameSite)
	}

	if session.Path != "/" {
		t.Errorf("Path = %q, want / so /v1/me sees it", session.Path)
	}

	if !strings.HasPrefix(session.Value, auth.SessionPrefix) {
		t.Errorf("the session value %q carries no prefix a scanner would know", session.Value)
	}

	if len(harness.sessions.live) != 1 {
		t.Errorf("the registry recorded %d sessions, want 1", len(harness.sessions.live))
	}
}

// Behind a terminating proxy this process is spoken to over plain HTTP while the browser
// is on https, and a session cookie sent without Secure there is one that will travel over
// http:// the next time somebody types the host without a scheme.
func TestSessionCookieIsSecureBehindAProxy(t *testing.T) {
	harness := newHarness(t)
	state, started := harness.begin(t)

	request := httptest.NewRequest(http.MethodGet,
		"/v1/auth/github/callback?code=the-code&state="+url.QueryEscape(state), nil)
	request.AddCookie(started)
	request.Header.Set("X-Forwarded-Proto", "https")

	recorder := httptest.NewRecorder()
	harness.handler.ServeHTTP(recorder, request)

	for _, cookie := range (&http.Response{Header: recorder.Header()}).Cookies() {
		if cookie.Name == "loom_session" && !cookie.Secure {
			t.Error("the session cookie went out without Secure behind an https proxy")
		}
	}
}

func TestSignOutEndsTheSession(t *testing.T) {
	harness := newHarness(t)
	session := harness.signIn(t)

	response := sendAs(t, harness.handler, http.MethodPost, "/v1/auth/signout", session, "")
	if response.Code != http.StatusOK {
		t.Fatalf("signing out = %d, want 200: %s", response.Code, response.Body)
	}

	if len(harness.sessions.live) != 0 {
		t.Errorf("%d sessions survived signing out", len(harness.sessions.live))
	}

	// and the session it held mints nothing afterwards
	minted := sendAs(t, harness.handler, http.MethodPost, "/v1/me/tokens", session, `{"name":"after"}`)
	if minted.Code != http.StatusUnauthorized {
		t.Errorf("minting after signing out = %d, want 401: %s", minted.Code, minted.Body)
	}
}

// Signing out with nothing to sign out of is what a browser does when its cookie has
// already expired. It is the state that was asked for, so it is not a failure.
func TestSignOutWithoutASessionSucceeds(t *testing.T) {
	harness := newHarness(t)

	response := send(t, harness.handler, http.MethodPost, "/v1/auth/signout", "", "")
	if response.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: %s", response.Code, response.Body)
	}
}
