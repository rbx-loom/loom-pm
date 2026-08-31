package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/rbx-loom/loom-pm/internal/auth"
	"github.com/rbx-loom/loom-pm/internal/index"
	"github.com/rbx-loom/loom-pm/internal/pkgname"
	"github.com/rbx-loom/loom-pm/internal/publish"
)

// ErrLastOwner is publish's sentinel, named here because this is where it is answered.
var ErrLastOwner = publish.ErrLastOwner

// Owners reads and changes who may publish a package. Every change is made on behalf of a
// user who must already be an owner.
type Owners interface {
	Owners(ctx context.Context, name pkgname.Name) ([]string, error)
	AddOwner(ctx context.Context, name pkgname.Name, login string, actorID int64) error
	RemoveOwner(ctx context.Context, name pkgname.Name, login string, actorID int64) error
}

// Tokens manages a user's own API tokens.
type Tokens interface {
	ListTokens(ctx context.Context, userID int64) ([]auth.TokenSummary, error)

	// CreateToken mints a token and answers it alongside its summary. The secret exists
	// outside its hash only in that return value.
	CreateToken(ctx context.Context, userID int64, name string) (string, auth.TokenSummary, error)

	// RevokeToken revokes one of the user's own tokens, answering auth.ErrNoSuchToken for
	// one that is not theirs as for one that does not exist.
	RevokeToken(ctx context.Context, userID, tokenID int64) error
}

// bodyLimit bounds the small JSON bodies these endpoints take. Publishing has its own,
// much larger, bound.
const bodyLimit = 4 << 10

func (a *API) serveOwners(w http.ResponseWriter, r *http.Request) {
	name, err := nameFrom(r)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}

	owners, err := a.Owners.Owners(r.Context(), name)
	switch {
	case errors.Is(err, index.ErrNotFound):
		a.fail(w, r, http.StatusNotFound, fmt.Sprintf("'%s' is not published here.", name))
		return
	case err != nil:
		a.internal(w, r, "read the owners", err)
		return
	}

	a.render(w, http.StatusOK, map[string]any{"name": name.String(), "owners": owners})
}

func (a *API) serveOwnerChange(add bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := a.authenticate(w, r)
		if !ok {
			return
		}

		name, err := nameFrom(r)
		if err != nil {
			a.fail(w, r, http.StatusBadRequest, err.Error())
			return
		}

		var body struct {
			Login string `json:"login"`
		}
		if !a.decode(w, r, &body) {
			return
		}

		login := strings.TrimSpace(body.Login)
		if login == "" {
			a.fail(w, r, http.StatusBadRequest, "name the owner to change, as {\"login\": \"...\"}.")
			return
		}

		change := a.Owners.AddOwner
		if !add {
			change = a.Owners.RemoveOwner
		}

		switch err := change(r.Context(), name, login, actor.ID); {
		case errors.Is(err, index.ErrNotFound):
			a.fail(w, r, http.StatusNotFound, fmt.Sprintf("'%s' is not published here.", name))
		case errors.Is(err, auth.ErrNoSuchUser):
			a.fail(w, r, http.StatusNotFound, fmt.Sprintf("'%s' has never signed in here, so they cannot own a package yet.", login))
		case errors.Is(err, publish.ErrNotOwned):
			a.fail(w, r, http.StatusForbidden, fmt.Sprintf("'%s' is not yours, so its owners are not yours to change.", name))
		case errors.Is(err, ErrLastOwner):
			a.fail(w, r, http.StatusConflict, fmt.Sprintf("'%s' would be left with no owner; add another before removing this one.", name))
		case err != nil:
			a.internal(w, r, "change the owners", err)
		default:
			a.render(w, http.StatusOK, map[string]any{"name": name.String(), "login": login, "owner": add})
		}
	}
}

func (a *API) serveTokenList(w http.ResponseWriter, r *http.Request) {
	user, ok := a.authenticateEither(w, r)
	if !ok {
		return
	}

	summaries, err := a.Tokens.ListTokens(r.Context(), user.ID)
	if err != nil {
		a.internal(w, r, "read your tokens", err)
		return
	}

	a.render(w, http.StatusOK, map[string]any{"tokens": renderTokens(summaries)})
}

func (a *API) serveTokenCreate(w http.ResponseWriter, r *http.Request) {
	user, ok := a.requireSession(w, r)
	if !ok {
		return
	}

	var body struct {
		Name string `json:"name"`
	}
	if !a.decode(w, r, &body) {
		return
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		a.fail(w, r, http.StatusBadRequest, "name the token so you can recognise it later, as {\"name\": \"...\"}.")
		return
	}

	presented, summary, err := a.Tokens.CreateToken(r.Context(), user.ID, name)
	if err != nil {
		a.internal(w, r, "issue a token", err)
		return
	}

	rendered := renderToken(summary)
	rendered["token"] = presented
	a.render(w, http.StatusCreated, rendered)
}

func (a *API) serveTokenRevoke(w http.ResponseWriter, r *http.Request) {
	user, ok := a.authenticateEither(w, r)
	if !ok {
		return
	}

	tokenID, err := strconv.ParseInt(r.PathValue("token"), 10, 64)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, fmt.Sprintf("'%s' is not a token id.", r.PathValue("token")))
		return
	}

	switch err := a.Tokens.RevokeToken(r.Context(), user.ID, tokenID); {
	case errors.Is(err, auth.ErrNoSuchToken):
		a.fail(w, r, http.StatusNotFound, "you have no token with that id.")
	case err != nil:
		a.internal(w, r, "revoke the token", err)
	default:
		a.render(w, http.StatusOK, map[string]any{"id": tokenID, "revoked": true})
	}
}

// requireSession resolves the signed-in user, and is what minting a token needs.
//
// A token deliberately does not satisfy it. A token that may mint tokens is one whose leak
// outlives its own revocation: whoever took it makes a second, and revoking the one that
// leaked changes nothing. Signing in is the thing somebody holding only a stolen token
// cannot do, which is what makes it the credential this asks for.
func (a *API) requireSession(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	user, err := a.sessionUser(r)
	switch {
	case err == nil:
		return user, true
	case !errors.Is(err, auth.ErrNoSession):
		a.internal(w, r, "check your sign-in", err)
		return auth.User{}, false
	}

	// a caller who already holds a good token is refused rather than challenged: there is
	// no repeating the request with the credential they have that would work
	status := http.StatusUnauthorized
	if _, err := a.Authenticator.Authenticate(r.Context(), r.Header.Get("Authorization")); err == nil {
		status = http.StatusForbidden
	}

	a.fail(w, r, status, a.mintingAdvice())
	return auth.User{}, false
}

// mintingAdvice says where a token comes from on this registry, which depends on whether
// anybody can sign in to it.
func (a *API) mintingAdvice() string {
	if a.Provider == nil || a.Users == nil || a.Sessions == nil {
		return "a token cannot mint another token, and signing in is not configured on this registry; ask whoever runs it for one."
	}

	return "a token cannot mint another token; sign in at /v1/auth/github to create one."
}

// authenticateEither accepts a sign-in or a token.
//
// Listing and revoking are things both a browser and the CLI do, and neither raises what
// its caller may already do: a token that can revoke itself is a leak whoever notices it
// can close, which is the opposite of the problem minting has.
func (a *API) authenticateEither(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	user, err := a.sessionUser(r)
	switch {
	case err == nil:
		return user, true
	case !errors.Is(err, auth.ErrNoSession):
		a.internal(w, r, "check your sign-in", err)
		return auth.User{}, false
	}

	return a.authenticate(w, r)
}

// decode reads a small JSON body, answering false when it has already reported why it
// could not. An absent body is a malformed one here: every caller of this needs a field.
func (a *API) decode(w http.ResponseWriter, r *http.Request, into any) bool {
	reader := http.MaxBytesReader(w, r.Body, bodyLimit)

	if err := json.NewDecoder(reader).Decode(into); err != nil {
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			a.fail(w, r, http.StatusRequestEntityTooLarge, "that request body is too large.")
		case errors.Is(err, io.EOF):
			a.fail(w, r, http.StatusBadRequest, "this needs a JSON body.")
		default:
			a.fail(w, r, http.StatusBadRequest, "the request body is not valid JSON.")
		}

		return false
	}

	return true
}

func renderTokens(summaries []auth.TokenSummary) []map[string]any {
	rendered := make([]map[string]any, 0, len(summaries))
	for _, summary := range summaries {
		rendered = append(rendered, renderToken(summary))
	}

	return rendered
}

func renderToken(summary auth.TokenSummary) map[string]any {
	rendered := map[string]any{
		"id":         summary.ID,
		"name":       summary.Name,
		"created_at": summary.CreatedAt.UTC(),
	}

	// a token that has never been used says so by omission rather than by a zero date
	if !summary.LastUsedAt.IsZero() {
		rendered["last_used_at"] = summary.LastUsedAt.UTC()
	}

	return rendered
}
