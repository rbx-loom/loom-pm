package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/rbx-loom/loom-pm/internal/catalog"
	"github.com/rbx-loom/loom-pm/internal/index"
)

func (a *API) serveSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	terms := query.Get("q")
	if terms == "" {
		a.fail(w, r, http.StatusBadRequest, "say what to search for, as ?q=...")
		return
	}

	limit, ok := a.number(w, r, query.Get("limit"), catalog.DefaultLimit)
	if !ok {
		return
	}

	offset, ok := a.number(w, r, query.Get("offset"), 0)
	if !ok {
		return
	}

	// bounded here rather than left to the store: this is where a hostile number arrives,
	// and every implementation behind the interface would otherwise have to remember
	limit, offset = catalog.Page(limit, offset)

	results, err := a.Catalog.Search(r.Context(), terms, limit, offset)
	if err != nil {
		a.internal(w, r, "search", err)
		return
	}

	rendered := make([]map[string]any, 0, len(results.Packages))
	for _, summary := range results.Packages {
		rendered = append(rendered, renderSummary(summary))
	}

	a.render(w, http.StatusOK, map[string]any{
		"query":    results.Query,
		"total":    results.Total,
		"packages": rendered,
	})
}

func (a *API) servePackage(w http.ResponseWriter, r *http.Request) {
	name, err := nameFrom(r)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}

	detail, err := a.Catalog.Detail(r.Context(), name)
	switch {
	case errors.Is(err, index.ErrNotFound):
		a.fail(w, r, http.StatusNotFound, fmt.Sprintf("'%s' is not published here.", name))
		return
	case err != nil:
		a.internal(w, r, "read the package", err)
		return
	}

	versions := make([]map[string]any, 0, len(detail.Versions))
	for _, version := range detail.Versions {
		versions = append(versions, map[string]any{
			"version":      version.Version.String(),
			"yanked":       version.Yanked,
			"published_at": version.PublishedAt.UTC(),
			"downloads":    version.Downloads,
			"size_bytes":   version.SizeBytes,
		})
	}

	rendered := map[string]any{
		"name":        detail.Name.String(),
		"description": detail.Description,
		"repository":  detail.Repository,
		"license":     detail.License,
		"authors":     detail.Authors,
		"owners":      detail.Owners,
		"downloads": map[string]any{
			"total":  detail.Downloads.Total,
			"recent": detail.Downloads.Recent,
			"days":   catalog.RecentDays,
		},
		"versions": versions,
	}

	if latest, ok := detail.Latest(); ok {
		rendered["latest"] = latest.Version.String()
	}

	a.render(w, http.StatusOK, rendered)
}

func renderSummary(summary catalog.Summary) map[string]any {
	return map[string]any{
		"name":        summary.Name.String(),
		"description": summary.Description,
		"latest":      summary.Latest.String(),
		"downloads":   summary.Downloads,
		"updated_at":  summary.UpdatedAt.UTC(),
	}
}

// number reads a paging parameter, answering false when it has already reported why it
// could not. Bounds are catalog.Page's to impose, not a caller's to choose.
func (a *API) number(w http.ResponseWriter, r *http.Request, text string, fallback int) (int, bool) {
	if text == "" {
		return fallback, true
	}

	value, err := strconv.Atoi(text)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, fmt.Sprintf("'%s' is not a number.", text))
		return 0, false
	}

	return value, true
}
