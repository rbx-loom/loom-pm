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
	"time"

	"github.com/rbx-loom/loom-pm/internal/auth"
	"github.com/rbx-loom/loom-pm/internal/catalog"
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

	// enough to hold the packages a busy registry revalidates, and small enough that the
	// documents themselves are not the thing that exhausts memory
	documentCacheSize = 2048

	// one publish a minute sustained, ten at once. A release train pushing a handful of
	// packages never notices; a loop filling the blob store does.
	publishEvery = time.Minute
	publishBurst = 10
)

// Yanker sets and clears a version's yanked mark. A yanked version stays downloadable, so
// that a lock file pinning it keeps installing; it is excluded only when resolution
// chooses anew.
type Yanker interface {
	Yank(ctx context.Context, name pkgname.Name, version semver.Version, yanked bool, userID int64) error
}

// Usage counts what the registry served. Both calls are fire-and-forget: a download is
// served whether or not anybody is counting it.
type Usage interface {
	Download(versionID int64)
	TokenUsed(hash []byte)
}

type Dependencies struct {
	Store         index.Store
	Blobs         storage.Blobs
	Publisher     *publish.Service
	Authenticator *auth.Authenticator
	Yanker        Yanker
	Owners        Owners
	Tokens        Tokens
	Provider      auth.Provider
	Users         Users
	Catalog       catalog.Store
	Usage         Usage
	Limits        publish.Limits

	// MetricsToken guards /metrics when set. Unset, the endpoint is open: it carries no
	// package names and no secrets, only how much of each kind of request was served.
	MetricsToken string

	Logger *slog.Logger
}

type API struct {
	Dependencies

	documents *index.Cache
	publishes *limiter
	metrics   *metrics
}

// New builds the handler.
//
// A missing logger or usage recorder is defaulted, because serving without either is still
// serving. A partially configured Limits panics instead: an unset bound is a zero one, and
// a registry that rejects every upload has failed in a way nothing downstream can report.
func New(dependencies Dependencies) http.Handler {
	if dependencies.Limits == (publish.Limits{}) {
		dependencies.Limits = publish.DefaultLimits()
	}
	if !dependencies.Limits.Valid() {
		panic("api: Limits is partially configured; set every bound or none")
	}
	if dependencies.Logger == nil {
		dependencies.Logger = slog.New(slog.DiscardHandler)
	}
	if dependencies.Usage == nil {
		dependencies.Usage = discardUsage{}
	}

	api := &API{
		Dependencies: dependencies,
		documents:    index.NewCache(documentCacheSize),
		publishes:    newLimiter(publishEvery, publishBurst),
		metrics:      newMetrics(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", api.serveHealth)
	mux.HandleFunc("GET /metrics", api.serveMetrics)

	mux.HandleFunc("GET /v1/index/{name}", api.serveIndex)
	mux.HandleFunc("GET /v1/index/{scope}/{name}", api.serveIndex)
	mux.HandleFunc("GET /v1/packages/{name}/{version}/download", api.serveDownload)
	mux.HandleFunc("GET /v1/packages/{scope}/{name}/{version}/download", api.serveDownload)

	mux.HandleFunc("POST /v1/publish", api.servePublish)

	mux.HandleFunc("PUT /v1/packages/{name}/{version}/yank", api.serveYank(true))
	mux.HandleFunc("PUT /v1/packages/{scope}/{name}/{version}/yank", api.serveYank(true))
	mux.HandleFunc("DELETE /v1/packages/{name}/{version}/yank", api.serveYank(false))
	mux.HandleFunc("DELETE /v1/packages/{scope}/{name}/{version}/yank", api.serveYank(false))

	mux.HandleFunc("GET /v1/packages/{name}/owners", api.serveOwners)
	mux.HandleFunc("GET /v1/packages/{scope}/{name}/owners", api.serveOwners)
	mux.HandleFunc("PUT /v1/packages/{name}/owners", api.serveOwnerChange(true))
	mux.HandleFunc("PUT /v1/packages/{scope}/{name}/owners", api.serveOwnerChange(true))
	mux.HandleFunc("DELETE /v1/packages/{name}/owners", api.serveOwnerChange(false))
	mux.HandleFunc("DELETE /v1/packages/{scope}/{name}/owners", api.serveOwnerChange(false))

	mux.HandleFunc("GET /v1/me/tokens", api.serveTokenList)
	mux.HandleFunc("POST /v1/me/tokens", api.serveTokenCreate)
	mux.HandleFunc("DELETE /v1/me/tokens/{token}", api.serveTokenRevoke)

	mux.HandleFunc("GET /v1/search", api.serveSearch)
	mux.HandleFunc("GET /v1/packages/{name}", api.servePackage)
	mux.HandleFunc("GET /v1/packages/{scope}/{name}", api.servePackage)

	mux.HandleFunc("GET /v1/auth/github", api.serveSignIn)
	mux.HandleFunc("GET /v1/auth/github/callback", api.serveSignInCallback)

	// registered under GET rather than bare, so a wrong method still resolves to 405
	// instead of being swallowed here as a 404
	mux.HandleFunc("GET /", api.serveUnknown)

	return observed(crossOrigin(mux), dependencies.Logger, api.metrics)
}

// serveHealth answers whether the process is running, and deliberately touches nothing
// else: a liveness check that fails when the database does gets the container restarted
// for someone else's outage.
func (a *API) serveHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok\n"))
}

func (a *API) serveIndex(w http.ResponseWriter, r *http.Request) {
	name, err := nameFrom(r)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// asked first, and on its own: revalidation is the common case here, and rebuilding a
	// document to answer "no, nothing changed" is the whole cost of the request
	modified, err := a.Store.Modified(r.Context(), name)
	switch {
	case errors.Is(err, index.ErrNotFound):
		a.fail(w, r, http.StatusNotFound, fmt.Sprintf("'%s' is not published here.", name))
		return
	case err != nil:
		a.internal(w, r, "read the index", err)
		return
	}

	document, cached := a.documents.Lookup(name.Normalized(), modified)
	a.metrics.indexCache(cached)

	if !cached {
		pkg, err := a.Store.Package(r.Context(), name)
		switch {
		case errors.Is(err, index.ErrNotFound):
			a.fail(w, r, http.StatusNotFound, fmt.Sprintf("'%s' is not published here.", name))
			return
		case err != nil:
			a.internal(w, r, "read the index", err)
			return
		}

		document, err = index.Build(pkg)
		if err != nil {
			a.internal(w, r, "render the index", err)
			return
		}

		a.documents.Store(name.Normalized(), modified, document)
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
		a.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}

	published, err := a.Store.Version(r.Context(), name, version)
	switch {
	case errors.Is(err, index.ErrNotFound):
		a.fail(w, r, http.StatusNotFound, fmt.Sprintf("'%s' %s is not published here.", name, version))
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
	header.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", downloadName(name, version)))
	header.Set("ETag", `"`+published.Checksum.Hex()+`"`)
	header.Set("Cache-Control", downloadCacheControl)

	// a ranged request is a download being resumed rather than started, and counting it
	// again would make a flaky connection look like a popular package
	if r.Header.Get("Range") == "" {
		a.Usage.Download(published.ID)
	}

	seeker, ok := content.(io.ReadSeeker)
	if !ok {
		// a backend that cannot seek gets the plain copy, and the client gets no Range
		header.Set("Content-Length", strconv.FormatInt(size, 10))
		if _, err := io.Copy(w, content); err != nil {
			a.Logger.WarnContext(r.Context(), "writing a package", "error", err,
				"package", name.String(), "version", version.String())
		}
		return
	}

	// ServeContent honours Range and revalidates against the ETag set above, so a download
	// that dies halfway resumes rather than starting over with a length it cannot meet
	http.ServeContent(w, r, downloadName(name, version), published.PublishedAt, seeker)
}

func (a *API) servePublish(w http.ResponseWriter, r *http.Request) {
	publisher, ok := a.authenticate(w, r)
	if !ok {
		return
	}

	if wait, allowed := a.publishes.allow(publisher.ID, time.Now()); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Round(time.Second)/time.Second)))
		a.fail(w, r, http.StatusTooManyRequests,
			fmt.Sprintf("this token is publishing too often; try again in %s.", wait.Round(time.Second)))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, a.Limits.CompressedBytes)
	content, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			a.fail(w, r, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("at most %d bytes may be published at once.", a.Limits.CompressedBytes))
			return
		}

		a.fail(w, r, http.StatusBadRequest, "the upload could not be read.")
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
		a.fail(w, r, http.StatusBadRequest, invalid.Error())
	case errors.Is(err, publish.ErrAlreadyPublished), errors.Is(err, publish.ErrSquatted):
		a.fail(w, r, http.StatusConflict, err.Error())
	case errors.Is(err, publish.ErrNotOwned), errors.Is(err, publish.ErrNotScopeMember):
		a.fail(w, r, http.StatusForbidden, err.Error())
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
			a.fail(w, r, http.StatusBadRequest, err.Error())
			return
		}

		switch err := a.Yanker.Yank(r.Context(), name, version, yanked, user.ID); {
		case errors.Is(err, index.ErrNotFound):
			a.fail(w, r, http.StatusNotFound, fmt.Sprintf("'%s' %s is not published here.", name, version))
		case errors.Is(err, publish.ErrNotOwned):
			a.fail(w, r, http.StatusForbidden, err.Error())
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
	a.fail(w, r, http.StatusNotFound, fmt.Sprintf("'%s' is not an endpoint of this registry.", r.URL.Path))
}

func (a *API) authenticate(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	user, err := a.Authenticator.Authenticate(r.Context(), r.Header.Get("Authorization"))
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		w.Header().Set("WWW-Authenticate", `Bearer realm="loom"`)
		a.fail(w, r, http.StatusUnauthorized, "this needs a valid API token; run 'loom login' to get one.")
		return auth.User{}, false
	case err != nil:
		a.internal(w, r, "check your token", err)
		return auth.User{}, false
	}

	a.Usage.TokenUsed(user.TokenHash)

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

	// the id the same failure was logged under, so a report of one can be found
	RequestID string `json:"request_id,omitempty"`
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

func (a *API) fail(w http.ResponseWriter, r *http.Request, status int, details ...string) {
	envelope := errorEnvelope{
		Errors:    make([]errorDetail, 0, len(details)),
		RequestID: requestIDOf(r.Context()),
	}
	for _, detail := range details {
		envelope.Errors = append(envelope.Errors, errorDetail{Detail: detail})
	}

	a.render(w, status, envelope)
}

// internal logs the cause and tells the client only what could not be done: an internal
// failure is not the caller's to read. The request id is on both, which is what connects
// the report to the log line.
func (a *API) internal(w http.ResponseWriter, r *http.Request, what string, err error) {
	a.Logger.ErrorContext(r.Context(), "serving a request", "error", err,
		"method", r.Method, "path", r.URL.Path, "request", requestIDOf(r.Context()))
	a.fail(w, r, http.StatusInternalServerError, "the registry could not "+what+" just now; try again shortly.")
}

type discardUsage struct{}

func (discardUsage) Download(int64)   {}
func (discardUsage) TokenUsed([]byte) {}
