package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// githubStub stands in for GitHub, so the exchange is exercised without one.
type githubStub struct {
	tokenStatus int
	tokenBody   string
	userStatus  int
	userBody    string

	tokenForm url.Values
	userAuth  string
}

func (g *githubStub) start(t *testing.T) *GitHub {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		g.tokenForm = r.PostForm

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(g.tokenStatus)
		w.Write([]byte(g.tokenBody))
	})

	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		g.userAuth = r.Header.Get("Authorization")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(g.userStatus)
		w.Write([]byte(g.userBody))
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	provider := NewGitHub("client", "secret", "https://registry.example/v1/auth/github/callback")
	provider.AuthorizeEndpoint = server.URL + "/login/oauth/authorize"
	provider.TokenEndpoint = server.URL + "/login/oauth/access_token"
	provider.UserEndpoint = server.URL + "/user"

	return provider
}

func workingStub() *githubStub {
	return &githubStub{
		tokenStatus: http.StatusOK,
		tokenBody:   `{"access_token":"gho_example","token_type":"bearer","scope":""}`,
		userStatus:  http.StatusOK,
		userBody:    `{"id":4242,"login":"ada","avatar_url":"https://avatars.example/ada.png"}`,
	}
}

func TestAuthorizeURL(t *testing.T) {
	stub := workingStub()
	provider := stub.start(t)

	parsed, err := url.Parse(provider.AuthorizeURL("the-state"))
	if err != nil {
		t.Fatalf("AuthorizeURL is not a URL: %v", err)
	}

	query := parsed.Query()
	if query.Get("client_id") != "client" {
		t.Errorf("client_id = %q, want client", query.Get("client_id"))
	}

	if query.Get("state") != "the-state" {
		t.Errorf("state = %q, want it carried", query.Get("state"))
	}

	if query.Get("redirect_uri") != "https://registry.example/v1/auth/github/callback" {
		t.Errorf("redirect_uri = %q, want the configured one", query.Get("redirect_uri"))
	}

	// the registry reads a public identity and nothing else, so it asks for nothing else
	if scope := query.Get("scope"); scope != "" && scope != "read:user" {
		t.Errorf("scope = %q, want no more than read:user", scope)
	}
}

func TestIdentify(t *testing.T) {
	stub := workingStub()
	provider := stub.start(t)

	identity, err := provider.Identify(context.Background(), "the-code")
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}

	if identity.GitHubID != 4242 || identity.Login != "ada" {
		t.Errorf("identity = %+v, want ada/4242", identity)
	}

	if identity.AvatarURL != "https://avatars.example/ada.png" {
		t.Errorf("avatar = %q, want the one GitHub gave", identity.AvatarURL)
	}

	if stub.tokenForm.Get("code") != "the-code" {
		t.Errorf("code sent = %q, want the-code", stub.tokenForm.Get("code"))
	}

	if stub.tokenForm.Get("client_secret") != "secret" {
		t.Error("the client secret was not sent to the token endpoint")
	}

	if !strings.Contains(stub.userAuth, "gho_example") {
		t.Errorf("the user endpoint was called with %q, want the access token", stub.userAuth)
	}
}

func TestIdentifyRefusals(t *testing.T) {
	cases := []struct {
		name    string
		adjust  func(*githubStub)
		because string
	}{
		{
			"the exchange is refused",
			func(s *githubStub) {
				s.tokenBody = `{"error":"bad_verification_code","error_description":"The code is incorrect."}`
			},
			"GitHub reported an error instead of a token",
		},
		{
			"the exchange returns no token",
			func(s *githubStub) { s.tokenBody = `{"token_type":"bearer"}` },
			"there is no access token to use",
		},
		{
			"the exchange fails",
			func(s *githubStub) { s.tokenStatus = http.StatusInternalServerError; s.tokenBody = `{}` },
			"GitHub could not be reached usefully",
		},
		{
			"the exchange is not JSON",
			func(s *githubStub) { s.tokenBody = `<html>nope</html>` },
			"the response cannot be read",
		},
		{
			"the identity is refused",
			func(s *githubStub) { s.userStatus = http.StatusUnauthorized; s.userBody = `{}` },
			"the token does not identify anybody",
		},
		{
			"the identity has no login",
			func(s *githubStub) { s.userBody = `{"id":4242}` },
			"an identity without a login cannot own anything",
		},
		{
			"the identity has no id",
			func(s *githubStub) { s.userBody = `{"login":"ada"}` },
			"the id is the identity; a login can be changed",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			stub := workingStub()
			testCase.adjust(stub)
			provider := stub.start(t)

			identity, err := provider.Identify(context.Background(), "the-code")
			if err == nil {
				t.Fatalf("Identify returned %+v, want an error because %s", identity, testCase.because)
			}
		})
	}
}

// The client secret must never reach whoever is signing in.
func TestIdentifyErrorsKeepTheSecret(t *testing.T) {
	stub := workingStub()
	stub.tokenBody = `{"error":"bad_verification_code"}`
	provider := stub.start(t)

	_, err := provider.Identify(context.Background(), "the-code")
	if err == nil {
		t.Fatal("Identify succeeded")
	}

	if strings.Contains(err.Error(), "secret") {
		t.Errorf("the error names the client secret: %v", err)
	}
}
