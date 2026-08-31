package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rbx-loom/loom-pm/internal/auth"
	"github.com/rbx-loom/loom-pm/internal/index"
	"github.com/rbx-loom/loom-pm/internal/pkgname"
	"github.com/rbx-loom/loom-pm/internal/publish"
)

type fakeOwners struct {
	owners map[string][]string
	err    error
}

func (f *fakeOwners) Owners(_ context.Context, name pkgname.Name) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}

	owners, ok := f.owners[name.Normalized()]
	if !ok {
		return nil, index.ErrNotFound
	}

	return owners, nil
}

func (f *fakeOwners) AddOwner(_ context.Context, name pkgname.Name, login string, _ int64) error {
	if f.err != nil {
		return f.err
	}

	f.owners[name.Normalized()] = append(f.owners[name.Normalized()], login)
	return nil
}

func (f *fakeOwners) RemoveOwner(_ context.Context, name pkgname.Name, login string, _ int64) error {
	if f.err != nil {
		return f.err
	}

	kept := make([]string, 0, len(f.owners[name.Normalized()]))
	for _, owner := range f.owners[name.Normalized()] {
		if owner != login {
			kept = append(kept, owner)
		}
	}

	f.owners[name.Normalized()] = kept
	return nil
}

type fakeTokens struct {
	summaries []auth.TokenSummary
	revoked   []int64
	err       error
}

func (f *fakeTokens) ListTokens(context.Context, int64) ([]auth.TokenSummary, error) {
	if f.err != nil {
		return nil, f.err
	}

	return f.summaries, nil
}

func (f *fakeTokens) CreateToken(_ context.Context, _ int64, name string) (string, auth.TokenSummary, error) {
	if f.err != nil {
		return "", auth.TokenSummary{}, f.err
	}

	presented, _, err := auth.NewToken()
	if err != nil {
		return "", auth.TokenSummary{}, err
	}

	summary := auth.TokenSummary{ID: 42, Name: name, CreatedAt: time.Date(2026, 3, 14, 9, 21, 0, 0, time.UTC)}
	f.summaries = append(f.summaries, summary)
	return presented, summary, nil
}

func (f *fakeTokens) RevokeToken(_ context.Context, _ int64, tokenID int64) error {
	if f.err != nil {
		return f.err
	}

	f.revoked = append(f.revoked, tokenID)
	return nil
}

// sendAs is send for a browser: a session cookie instead of an Authorization header.
func sendAs(t *testing.T, handler http.Handler, method, target string, session *http.Cookie, body string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	request := httptest.NewRequest(method, target, reader)
	request.AddCookie(session)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func send(t *testing.T, handler http.Handler, method, target, token, body string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	request := httptest.NewRequest(method, target, reader)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestOwnersLists(t *testing.T) {
	harness := newHarness(t)

	response := send(t, harness.handler, http.MethodGet, "/v1/packages/serio/owners", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body)
	}

	var listed struct {
		Owners []string `json:"owners"`
	}
	if err := json.NewDecoder(response.Body).Decode(&listed); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	if strings.Join(listed.Owners, ",") != "ada" {
		t.Errorf("owners = %v, want ada", listed.Owners)
	}
}

func TestOwnersListsAScopedPackage(t *testing.T) {
	harness := newHarness(t)
	harness.owners.owners["scope/serio"] = []string{"ada"}

	if response := send(t, harness.handler, http.MethodGet, "/v1/packages/scope/serio/owners", "", ""); response.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", response.Code)
	}
}

func TestOwnersUnknownPackage(t *testing.T) {
	harness := newHarness(t)

	if response := send(t, harness.handler, http.MethodGet, "/v1/packages/nothing/owners", "", ""); response.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", response.Code)
	}
}

func TestOwnersAdds(t *testing.T) {
	harness := newHarness(t)

	response := send(t, harness.handler, http.MethodPut, "/v1/packages/serio/owners", harness.token, `{"login":"grace"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body)
	}

	if strings.Join(harness.owners.owners["serio"], ",") != "ada,grace" {
		t.Errorf("owners = %v, want grace added", harness.owners.owners["serio"])
	}
}

func TestOwnersRemoves(t *testing.T) {
	harness := newHarness(t)
	harness.owners.owners["serio"] = []string{"ada", "grace"}

	response := send(t, harness.handler, http.MethodDelete, "/v1/packages/serio/owners", harness.token, `{"login":"grace"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body)
	}

	if strings.Join(harness.owners.owners["serio"], ",") != "ada" {
		t.Errorf("owners = %v, want grace removed", harness.owners.owners["serio"])
	}
}

func TestOwnersMutationRequiresAToken(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			harness := newHarness(t)

			response := send(t, harness.handler, method, "/v1/packages/serio/owners", "", `{"login":"grace"}`)
			if response.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", response.Code)
			}
		})
	}
}

func TestOwnersMutationRefusals(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"not an owner", publish.ErrNotOwned, http.StatusForbidden},
		{"unknown package", index.ErrNotFound, http.StatusNotFound},
		{"unknown login", auth.ErrNoSuchUser, http.StatusNotFound},
		{"the last owner", ErrLastOwner, http.StatusConflict},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newHarness(t)
			harness.owners.err = testCase.err

			response := send(t, harness.handler, http.MethodPut, "/v1/packages/serio/owners", harness.token, `{"login":"grace"}`)
			if response.Code != testCase.status {
				t.Errorf("status = %d, want %d", response.Code, testCase.status)
			}

			if details := decodeErrors(t, response.Body); len(details) == 0 {
				t.Error("the refusal carried no diagnostic")
			}
		})
	}
}

func TestOwnersRejectsABadBody(t *testing.T) {
	cases := []struct{ name, body string }{
		{"not json", `nonsense`},
		{"no login", `{}`},
		{"blank login", `{"login":"   "}`},
		{"empty body", ``},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newHarness(t)

			response := send(t, harness.handler, http.MethodPut, "/v1/packages/serio/owners", harness.token, testCase.body)
			if response.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", response.Code)
			}
		})
	}
}

func TestTokensList(t *testing.T) {
	harness := newHarness(t)
	harness.tokens.summaries = []auth.TokenSummary{{ID: 1, Name: "laptop"}}

	response := send(t, harness.handler, http.MethodGet, "/v1/me/tokens", harness.token, "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body)
	}

	body := response.Body.String()
	if !strings.Contains(body, "laptop") {
		t.Errorf("the listing %s does not name the token", body)
	}

	// a listing must never carry the secret, only what identifies it
	if strings.Contains(body, auth.TokenPrefix) {
		t.Errorf("the listing %s carries a token", body)
	}
}

// Minting takes the sign-in, which is the whole point of refusing the token: the
// credential that makes credentials is the one somebody holding a stolen token cannot get.
func TestTokensCreateShowsTheSecretOnce(t *testing.T) {
	harness := newHarness(t)
	session := harness.signIn(t)

	response := sendAs(t, harness.handler, http.MethodPost, "/v1/me/tokens", session, `{"name":"ci"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", response.Code, response.Body)
	}

	var created struct {
		Token string `json:"token"`
		Name  string `json:"name"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	if !strings.HasPrefix(created.Token, auth.TokenPrefix) {
		t.Errorf("token = %q, want a real one", created.Token)
	}

	if created.Name != "ci" {
		t.Errorf("name = %q, want ci", created.Name)
	}

	listing := send(t, harness.handler, http.MethodGet, "/v1/me/tokens", harness.token, "")
	if strings.Contains(listing.Body.String(), created.Token) {
		t.Error("the token is readable again from the listing")
	}
}

func TestTokensCreateRequiresAName(t *testing.T) {
	harness := newHarness(t)
	session := harness.signIn(t)

	for _, body := range []string{`{}`, `{"name":"  "}`, ``, `nonsense`} {
		t.Run(body, func(t *testing.T) {
			response := sendAs(t, harness.handler, http.MethodPost, "/v1/me/tokens", session, body)
			if response.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", response.Code)
			}
		})
	}
}

func TestTokensRevoke(t *testing.T) {
	harness := newHarness(t)

	response := send(t, harness.handler, http.MethodDelete, "/v1/me/tokens/42", harness.token, "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body)
	}

	if len(harness.tokens.revoked) != 1 || harness.tokens.revoked[0] != 42 {
		t.Errorf("revoked %v, want [42]", harness.tokens.revoked)
	}
}

func TestTokensRevokeUnknown(t *testing.T) {
	harness := newHarness(t)
	harness.tokens.err = auth.ErrNoSuchToken

	if response := send(t, harness.handler, http.MethodDelete, "/v1/me/tokens/42", harness.token, ""); response.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", response.Code)
	}
}

func TestTokensRevokeRejectsABadID(t *testing.T) {
	harness := newHarness(t)

	if response := send(t, harness.handler, http.MethodDelete, "/v1/me/tokens/not-a-number", harness.token, ""); response.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", response.Code)
	}
}

func TestTokensRequireAToken(t *testing.T) {
	harness := newHarness(t)

	cases := []struct{ method, target, body string }{
		{http.MethodGet, "/v1/me/tokens", ""},
		{http.MethodPost, "/v1/me/tokens", `{"name":"ci"}`},
		{http.MethodDelete, "/v1/me/tokens/42", ""},
	}

	for _, testCase := range cases {
		t.Run(testCase.method, func(t *testing.T) {
			response := send(t, harness.handler, testCase.method, testCase.target, "", testCase.body)
			if response.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", response.Code)
			}
		})
	}
}

// A token may not mint another. Were it able to, a leaked token would outlive the
// revocation of itself: whoever took it makes a second before anybody notices, and
// revoking the one that leaked closes nothing.
//
// It is refused rather than challenged, because there is no token the caller could send
// instead that would work.
func TestTokensCannotMintTokens(t *testing.T) {
	harness := newHarness(t)

	response := send(t, harness.handler, http.MethodPost, "/v1/me/tokens", harness.token, `{"name":"second"}`)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", response.Code, response.Body)
	}

	if !strings.Contains(response.Body.String(), "/v1/auth/github") {
		t.Errorf("the refusal does not say where a token comes from: %s", response.Body)
	}

	if len(harness.tokens.summaries) != 0 {
		t.Errorf("a token was minted anyway: %v", harness.tokens.summaries)
	}
}

// A registry nobody can sign in to still refuses, and says the only other way a token is
// made there: the operator's command line.
func TestTokenMintingWithoutSignInSaysSo(t *testing.T) {
	harness := newHarnessWithout(t)

	response := send(t, harness.handler, http.MethodPost, "/v1/me/tokens", harness.token, `{"name":"second"}`)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", response.Code, response.Body)
	}

	if !strings.Contains(response.Body.String(), "ask whoever runs it") {
		t.Errorf("the refusal does not say where a token comes from: %s", response.Body)
	}
}

// An expired session is no session: it mints nothing, and says the same as none at all.
func TestAnExpiredSessionMintsNothing(t *testing.T) {
	harness := newHarness(t)
	session := harness.signIn(t)

	harness.sessions.now = harness.sessions.now.Add(sessionLifetime + time.Hour)

	response := sendAs(t, harness.handler, http.MethodPost, "/v1/me/tokens", session, `{"name":"second"}`)
	if response.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401: %s", response.Code, response.Body)
	}
}

// Listing and revoking still take a token. Neither raises what its caller may already do,
// and revoking is how somebody closes a leak they have just noticed — from wherever they
// noticed it.
func TestListingAndRevokingStillTakeAToken(t *testing.T) {
	harness := newHarness(t)

	if response := send(t, harness.handler, http.MethodGet, "/v1/me/tokens", harness.token, ""); response.Code != http.StatusOK {
		t.Errorf("listing = %d, want 200: %s", response.Code, response.Body)
	}

	if response := send(t, harness.handler, http.MethodDelete, "/v1/me/tokens/42", harness.token, ""); response.Code != http.StatusOK {
		t.Errorf("revoking = %d, want 200: %s", response.Code, response.Body)
	}
}

// A session reaches them too, so a browser is not required to hold a token to manage its
// own.
func TestListingAndRevokingTakeASession(t *testing.T) {
	harness := newHarness(t)
	session := harness.signIn(t)

	if response := sendAs(t, harness.handler, http.MethodGet, "/v1/me/tokens", session, ""); response.Code != http.StatusOK {
		t.Errorf("listing = %d, want 200: %s", response.Code, response.Body)
	}

	if response := sendAs(t, harness.handler, http.MethodDelete, "/v1/me/tokens/42", session, ""); response.Code != http.StatusOK {
		t.Errorf("revoking = %d, want 200: %s", response.Code, response.Body)
	}
}
