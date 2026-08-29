package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"html/template"
	"net/http"

	"github.com/rbx-loom/loom-pm/internal/auth"
)

// stateCookie carries the value the callback has to hand back. It is the only thing
// separating a callback the user started from one somebody else did.
const stateCookie = "loom_oauth_state"

// statePath scopes the cookie to the sign-in endpoints, so it is not attached to every
// publish and download.
const statePath = "/v1/auth"

// Users records who has signed in.
type Users interface {
	// UpsertGitHubUser answers the row id of the user an identity belongs to, creating or
	// updating it. GitHubID is the identity; a login is a display name that changes.
	UpsertGitHubUser(ctx context.Context, identity auth.Identity) (int64, error)
}

func (a *API) serveSignIn(w http.ResponseWriter, r *http.Request) {
	if !a.signInConfigured(w, r) {
		return
	}

	state, err := newState()
	if err != nil {
		a.internal(w, r, "start a sign-in", err)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     stateCookie,
		Value:    state,
		Path:     statePath,
		MaxAge:   600,
		HttpOnly: true,
		Secure:   r.TLS != nil,

		// Lax rather than Strict: the callback arrives as a redirect from GitHub, and
		// Strict would withhold the cookie on exactly that request
		SameSite: http.SameSiteLaxMode,
	})

	http.Redirect(w, r, a.Provider.AuthorizeURL(state), http.StatusFound)
}

func (a *API) serveSignInCallback(w http.ResponseWriter, r *http.Request) {
	if !a.signInConfigured(w, r) {
		return
	}

	// spent whatever happens next, so a state cannot be replayed
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: "", Path: statePath, MaxAge: -1, HttpOnly: true, Secure: r.TLS != nil,
	})

	cookie, err := r.Cookie(stateCookie)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, "this sign-in did not start here, or it took too long; start it again.")
		return
	}

	presented := r.URL.Query().Get("state")
	if presented == "" || subtle.ConstantTimeCompare([]byte(presented), []byte(cookie.Value)) != 1 {
		a.fail(w, r, http.StatusBadRequest, "this sign-in did not start here; start it again.")
		return
	}

	if refused := r.URL.Query().Get("error"); refused != "" {
		description := r.URL.Query().Get("error_description")
		if description == "" {
			description = refused
		}

		a.fail(w, r, http.StatusBadRequest, fmt.Sprintf("GitHub did not complete the sign-in: %s", description))
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		a.fail(w, r, http.StatusBadRequest, "GitHub returned no code, so there is nothing to complete.")
		return
	}

	identity, err := a.Provider.Identify(r.Context(), code)
	if err != nil {
		a.Logger.ErrorContext(r.Context(), "identifying a sign-in", "error", err)
		a.fail(w, r, http.StatusBadGateway, "GitHub could not confirm the sign-in just now; try again shortly.")
		return
	}

	userID, err := a.Users.UpsertGitHubUser(r.Context(), identity)
	if err != nil {
		a.internal(w, r, "record the sign-in", err)
		return
	}

	token, _, err := a.Tokens.CreateToken(r.Context(), userID, "web sign-in")
	if err != nil {
		a.internal(w, r, "issue a token", err)
		return
	}

	a.renderToken(w, r, identity.Login, token)
}

func (a *API) signInConfigured(w http.ResponseWriter, r *http.Request) bool {
	if a.Provider == nil || a.Users == nil {
		a.fail(w, r, http.StatusServiceUnavailable,
			"signing in is not configured on this registry; ask whoever runs it for a token.")
		return false
	}

	return true
}

func newState() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("api: generating a sign-in state: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// tokenPage is what a browser lands on. The token exists outside its hash only here, so
// the page says so and nothing caches it.
var tokenPage = template.Must(template.New("token").Parse(`<!doctype html>
<html lang="en">
<meta charset="utf-8">
<title>Signed in to Loom</title>
<style>
 body { font: 16px/1.5 system-ui, sans-serif; margin: 4rem auto; max-width: 42rem; padding: 0 1rem; }
 code { background: #f4f4f5; border-radius: 4px; padding: 0.15em 0.4em; }
 pre { background: #f4f4f5; border-radius: 6px; padding: 1rem; overflow-x: auto; user-select: all; }
</style>
<h1>Signed in as {{.Login}}</h1>
<p>This is your API token. It is shown once and cannot be read again — store it now.</p>
<pre>{{.Token}}</pre>
<p>Give it to the CLI with <code>loom login</code>, and revoke it with
<code>DELETE /v1/me/tokens/&lt;id&gt;</code> if it ever leaks.</p>
</html>
`))

func (a *API) renderToken(w http.ResponseWriter, r *http.Request, login, token string) {
	header := w.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	header.Set("Referrer-Policy", "no-referrer")

	page := struct{ Login, Token string }{Login: login, Token: token}
	if err := tokenPage.Execute(w, page); err != nil {
		a.Logger.ErrorContext(r.Context(), "rendering the sign-in page", "error", err)
	}
}
