// Package api serves the registry over HTTP.
//
// Errors are rendered as {"errors": [{"detail": "..."}]}, carrying text the CLI prints
// verbatim, so a registry refusal reads like every other Loom diagnostic.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/rbx-loom/loom-pm/internal/auth"
	"github.com/rbx-loom/loom-pm/internal/index"
	"github.com/rbx-loom/loom-pm/internal/pkgname"
	"github.com/rbx-loom/loom-pm/internal/publish"
	"github.com/rbx-loom/loom-pm/internal/semver"
	"github.com/rbx-loom/loom-pm/internal/storage"
)

const (
	// short, because a newly published version should be resolvable promptly; the
	// tarballs the document points at are the immutable part
	indexCacheControl = "public, max-age=60, stale-while-revalidate=600"

	// a published version is never replaced
	downloadCacheControl = "public, max-age=31536000, immutable"
)

// Yanker sets and clears a version's yanked mark. A yanked version stays downloadable, so
// that a lock file pinning it keeps installing; it is excluded only when resolution
// chooses anew.
type Yanker interface {
	Yank(ctx context.Context, name pkgname.Name, version semver.Version, yanked bool, userID int64) error
}

type Dependencies struct {
	Store         index.Store
	Blobs         storage.Blobs
	Publisher     *publish.Service
	Authenticator *auth.Authenticator
	Yanker        Yanker
	Limits        publish.Limits
	Logger        *slog.Logger
}

type API struct {
	Dependencies
}

func New(dependencies Dependencies) http.Handler {
	if dependencies.Limits == (publish.Limits{}) {
		dependencies.Limits = publish.DefaultLimits()
	}

	api := &API{Dependencies: dependencies}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/index/{name}", api.serveIndex)
	mux.HandleFunc("GET /v1/index/{scope}/{name}", api.serveIndex)
	mux.HandleFunc("GET /v1/packages/{name}/{version}/download", api.serveDownload)
	mux.HandleFunc("GET /v1/packages/{scope}/{name}/{version}/download", api.serveDownload)

	mux.HandleFunc("POST /v1/publish", api.servePublish)

	mux.HandleFunc("PUT /v1/packages/{name}/{version}/yank", api.serveYank(true))
	mux.HandleFunc("PUT /v1/packages/{scope}/{name}/{version}/yank", api.serveYank(true))
	mux.HandleFunc("DELETE /v1/packages/{name}/{version}/yank", api.serveYank(false))
	mux.HandleFunc("DELETE /v1/packages/{scope}/{name}/{version}/yank", api.serveYank(false))

	// registered under GET rather than bare, so a wrong method still resolves to 405
	// instead of being swallowed here as a 404
	mux.HandleFunc("GET /", api.serveUnknown)

	return mux
}

func (a *API) serveIndex(w http.ResponseWriter, r *http.Request) {
	name, err := nameFrom(r)
	if err != nil {
		a.fail(w, http.StatusBadRequest, err.Error())
		return
	}

	pkg, err := a.Store.Package(r.Context(), name)
	switch {
	case errors.Is(err, index.ErrNotFound):
		a.fail(w, http.StatusNotFound, fmt.Sprintf("'%s' is not published here.", name))
		return
	case err != nil:
		a.internal(w, r, "read the index", err)
		return
	}

	document, err := index.Build(pkg)
	if err != nil {
		a.internal(w, r, "render the index", err)
		return
	}

	header := w.Header()
	header.Set("ETag", document.ETag)
	header.Set("Cache-Control", indexCacheControl)

	if matches(r.Header.Get("If-None-Match"), document.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	header.Set("Content-Type", "application/json")
	header.Set("Content-Length", strconv.Itoa(len(document.Body)))
	if _, err := w.Write(document.Body); err != nil {
		a.Logger.WarnContext(r.Context(), "writing the index document", "error", err, "package", name.String())
	}
}

func (a *API) serveDownload(w http.ResponseWriter, r *http.Request) {
	name, version, err := versionFrom(r)
	if err != nil {
		a.fail(w, http.StatusBadRequest, err.Error())
		return
	}

	published, err := a.Store.Version(r.Context(), name, version)
	switch {
	case errors.Is(err, index.ErrNotFound):
		a.fail(w, http.StatusNotFound, fmt.Sprintf("'%s' %s is not published here.", name, version))
		return
	case err != nil:
		a.internal(w, r, "read the index", err)
		return
	}

	// a version row whose blob is gone is a broken package rather than a missing one, so
	// it is reported as the registry's failure and not as a 404
	content, size, err := a.Blobs.Open(r.Context(), published.Checksum)
	if err != nil {
		a.internal(w, r, "read the package", err)
		return
	}
	defer content.Close()

	header := w.Header()
	header.Set("Content-Type", "application/gzip")
	header.Set("Content-Length", strconv.FormatInt(size, 10))
	header.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", downloadName(name, version)))
	header.Set("ETag", `"`+published.Checksum.Hex()+`"`)
	header.Set("Cache-Control", downloadCacheControl)

	if _, err := io.Copy(w, content); err != nil {
		a.Logger.WarnContext(r.Context(), "writing a package", "error", err, "package", name.String(), "version", version.String())
	}
}

func (a *API) servePublish(w http.ResponseWriter, r *http.Request) {
	publisher, ok := a.authenticate(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, a.Limits.CompressedBytes)
	content, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			a.fail(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("at most %d bytes may be published at once.", a.Limits.CompressedBytes))
			return
		}

		a.fail(w, http.StatusBadRequest, "the upload could not be read.")
		return
	}

	payload, err := a.Publisher.Publish(r.Context(), content, publisher.ID)
	if err != nil {
		a.reportPublishFailure(w, r, err)
		return
	}

	published := payload.Manifest.Package
	a.render(w, http.StatusCreated, map[string]string{
		"name":     published.Name.String(),
		"version":  published.Version.String(),
		"checksum": payload.Digest.String(),
	})
}

func (a *API) reportPublishFailure(w http.ResponseWriter, r *http.Request, err error) {
	var invalid *publish.InvalidUpload

	switch {
	case errors.As(err, &invalid):
		a.fail(w, http.StatusBadRequest, invalid.Error())
	case errors.Is(err, publish.ErrAlreadyPublished), errors.Is(err, publish.ErrSquatted):
		a.fail(w, http.StatusConflict, err.Error())
	case errors.Is(err, publish.ErrNotOwned), errors.Is(err, publish.ErrNotScopeMember):
		a.fail(w, http.StatusForbidden, err.Error())
	default:
		a.internal(w, r, "publish this version", err)
	}
}

func (a *API) serveYank(yanked bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := a.authenticate(w, r)
		if !ok {
			return
		}

		name, version, err := versionFrom(r)
		if err != nil {
			a.fail(w, http.StatusBadRequest, err.Error())
			return
		}

		switch err := a.Yanker.Yank(r.Context(), name, version, yanked, user.ID); {
		case errors.Is(err, index.ErrNotFound):
			a.fail(w, http.StatusNotFound, fmt.Sprintf("'%s' %s is not published here.", name, version))
		case errors.Is(err, publish.ErrNotOwned):
			a.fail(w, http.StatusForbidden, err.Error())
		case err != nil:
			a.internal(w, r, "change this version", err)
		default:
			a.render(w, http.StatusOK, map[string]any{
				"name":    name.String(),
				"version": version.String(),
				"yanked":  yanked,
			})
		}
	}
}

func (a *API) serveUnknown(w http.ResponseWriter, r *http.Request) {
	a.fail(w, http.StatusNotFound, fmt.Sprintf("'%s' is not an endpoint of this registry.", r.URL.Path))
}

func (a *API) authenticate(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	user, err := a.Authenticator.Authenticate(r.Context(), r.Header.Get("Authorization"))
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		w.Header().Set("WWW-Authenticate", `Bearer realm="loom"`)
		a.fail(w, http.StatusUnauthorized, "this needs a valid API token; run 'loom login' to get one.")
		return auth.User{}, false
	case err != nil:
		a.internal(w, r, "check your token", err)
		return auth.User{}, false
	}

	return user, true
}

func nameFrom(r *http.Request) (pkgname.Name, error) {
	text := r.PathValue("name")
	if scope := r.PathValue("scope"); scope != "" {
		text = scope + "/" + text
	}

	return pkgname.Parse(text)
}

func versionFrom(r *http.Request) (pkgname.Name, semver.Version, error) {
	name, err := nameFrom(r)
	if err != nil {
		return pkgname.Name{}, semver.Version{}, err
	}

	version, err := semver.ParseVersion(r.PathValue("version"))
	if err != nil {
		return pkgname.Name{}, semver.Version{}, err
	}

	return name, version, nil
}

func downloadName(name pkgname.Name, version semver.Version) string {
	return strings.ReplaceAll(name.String(), "/", "-") + "-" + version.String() + ".tar.gz"
}

// matches reads an If-None-Match header, which may list several tags and may weaken them.
func matches(header, etag string) bool {
	for candidate := range strings.SplitSeq(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}

	return false
}

type errorEnvelope struct {
	Errors []errorDetail `json:"errors"`
}

type errorDetail struct {
	Detail string `json:"detail"`
}

func (a *API) render(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		http.Error(w, `{"errors":[{"detail":"the registry could not render this response."}]}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	w.Write(body)
}

func (a *API) fail(w http.ResponseWriter, status int, details ...string) {
	envelope := errorEnvelope{Errors: make([]errorDetail, 0, len(details))}
	for _, detail := range details {
		envelope.Errors = append(envelope.Errors, errorDetail{Detail: detail})
	}

	a.render(w, status, envelope)
}

// internal logs the cause and tells the client only what could not be done: an internal
// failure is not the caller's to read.
func (a *API) internal(w http.ResponseWriter, r *http.Request, what string, err error) {
	a.Logger.ErrorContext(r.Context(), "serving a request", "error", err, "method", r.Method, "path", r.URL.Path)
	a.fail(w, http.StatusInternalServerError, "the registry could not "+what+" just now; try again shortly.")
}
