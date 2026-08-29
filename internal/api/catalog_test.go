package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rbx-loom/loom-pm/internal/catalog"
	"github.com/rbx-loom/loom-pm/internal/index"
	"github.com/rbx-loom/loom-pm/internal/pkgname"
	"github.com/rbx-loom/loom-pm/internal/semver"
)

type fakeCatalog struct {
	packages map[string]catalog.Detail
	lastLimit,
	lastOffset int
	lastQuery string
	err       error
}

func (f *fakeCatalog) Search(_ context.Context, query string, limit, offset int) (catalog.Results, error) {
	if f.err != nil {
		return catalog.Results{}, f.err
	}

	f.lastQuery, f.lastLimit, f.lastOffset = query, limit, offset

	results := catalog.Results{Query: query, Packages: []catalog.Summary{}}
	for _, detail := range f.packages {
		if strings.Contains(detail.Name.Normalized(), strings.ToLower(query)) {
			results.Packages = append(results.Packages, summaryOf(detail))
			results.Total++
		}
	}

	return results, nil
}

func (f *fakeCatalog) Recent(_ context.Context, limit int) ([]catalog.Summary, error) {
	if f.err != nil {
		return nil, f.err
	}

	f.lastLimit = limit

	listed := []catalog.Summary{}
	for _, detail := range f.packages {
		listed = append(listed, summaryOf(detail))
	}

	return listed, nil
}

func (f *fakeCatalog) Detail(_ context.Context, name pkgname.Name) (catalog.Detail, error) {
	if f.err != nil {
		return catalog.Detail{}, f.err
	}

	detail, ok := f.packages[name.Normalized()]
	if !ok {
		return catalog.Detail{}, index.ErrNotFound
	}

	return detail, nil
}

func summaryOf(detail catalog.Detail) catalog.Summary {
	summary := catalog.Summary{
		Name:        detail.Name,
		Description: detail.Description,
		Downloads:   detail.Downloads.Total,
		UpdatedAt:   time.Date(2026, 3, 14, 9, 21, 0, 0, time.UTC),
	}

	if latest, ok := detail.Latest(); ok {
		summary.Latest = latest.Version
	}

	return summary
}

func catalogWith(t *testing.T, names ...string) *fakeCatalog {
	t.Helper()

	held := map[string]catalog.Detail{}
	for _, text := range names {
		name, err := pkgname.Parse(text)
		if err != nil {
			t.Fatalf("pkgname.Parse(%q): %v", text, err)
		}

		version, err := semver.ParseVersion("1.2.0")
		if err != nil {
			t.Fatalf("semver.ParseVersion: %v", err)
		}

		held[name.Normalized()] = catalog.Detail{
			Name:        name,
			Description: "A package called " + text,
			Repository:  "https://github.com/rbx-loom/" + name.Name(),
			License:     "Apache-2.0",
			Authors:     []string{"ada"},
			Owners:      []string{"ada"},
			Downloads:   catalog.Downloads{Total: 42, Recent: 7},
			Versions: []catalog.Version{{
				Version:     version,
				PublishedAt: time.Date(2026, 3, 14, 9, 21, 0, 0, time.UTC),
				Downloads:   42,
				SizeBytes:   1234,
			}},
		}
	}

	return &fakeCatalog{packages: held}
}

func TestSearchEndpoint(t *testing.T) {
	harness := newHarness(t)

	response := send(t, harness.handler, http.MethodGet, "/v1/search?q=serio", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body)
	}

	var results struct {
		Query    string `json:"query"`
		Total    int    `json:"total"`
		Packages []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Latest      string `json:"latest"`
			Downloads   int64  `json:"downloads"`
		} `json:"packages"`
	}
	if err := json.NewDecoder(response.Body).Decode(&results); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	if results.Query != "serio" || results.Total != 1 || len(results.Packages) != 1 {
		t.Fatalf("results = %+v, want one match for serio", results)
	}

	found := results.Packages[0]
	if found.Name != "serio" || found.Latest != "1.2.0" || found.Downloads != 42 {
		t.Errorf("found %+v, want serio 1.2.0 with its downloads", found)
	}
}

func TestSearchEndpointNeedsAQuery(t *testing.T) {
	harness := newHarness(t)

	if response := send(t, harness.handler, http.MethodGet, "/v1/search", "", ""); response.Code != http.StatusBadRequest {
		t.Errorf("a search with no q = %d, want 400", response.Code)
	}
}

// A caller asking for a million results gets a page, not the registry.
func TestSearchEndpointBoundsThePage(t *testing.T) {
	harness := newHarness(t)

	send(t, harness.handler, http.MethodGet, "/v1/search?q=serio&limit=100000&offset=-5", "", "")

	if harness.catalog.lastLimit > catalog.MaxLimit {
		t.Errorf("limit = %d, want it capped at %d", harness.catalog.lastLimit, catalog.MaxLimit)
	}

	if harness.catalog.lastOffset < 0 {
		t.Errorf("offset = %d, want it brought to zero", harness.catalog.lastOffset)
	}
}

func TestSearchEndpointRejectsNonsenseParameters(t *testing.T) {
	harness := newHarness(t)

	for _, query := range []string{"?q=serio&limit=abc", "?q=serio&offset=abc"} {
		t.Run(query, func(t *testing.T) {
			if response := send(t, harness.handler, http.MethodGet, "/v1/search"+query, "", ""); response.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", response.Code)
			}
		})
	}
}

func TestPackageEndpoint(t *testing.T) {
	harness := newHarness(t)

	response := send(t, harness.handler, http.MethodGet, "/v1/packages/serio", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body)
	}

	var detail struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Repository  string   `json:"repository"`
		Owners      []string `json:"owners"`
		Downloads   struct {
			Total  int64 `json:"total"`
			Recent int64 `json:"recent"`
		} `json:"downloads"`
		Versions []struct {
			Version   string `json:"version"`
			Downloads int64  `json:"downloads"`
			SizeBytes int64  `json:"size_bytes"`
		} `json:"versions"`
	}
	if err := json.NewDecoder(response.Body).Decode(&detail); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	if detail.Name != "serio" || detail.Repository == "" {
		t.Errorf("detail = %+v, want serio with its metadata", detail)
	}

	if detail.Downloads.Total != 42 || detail.Downloads.Recent != 7 {
		t.Errorf("downloads = %+v, want 42 total and 7 recent", detail.Downloads)
	}

	if len(detail.Versions) != 1 || detail.Versions[0].SizeBytes != 1234 {
		t.Errorf("versions = %+v, want the one with its size", detail.Versions)
	}

	if strings.Join(detail.Owners, ",") != "ada" {
		t.Errorf("owners = %v, want ada", detail.Owners)
	}
}

func TestPackageEndpointScoped(t *testing.T) {
	harness := newHarness(t)
	harness.catalog.packages = catalogWith(t, "scope/serio").packages

	if response := send(t, harness.handler, http.MethodGet, "/v1/packages/scope/serio", "", ""); response.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", response.Code)
	}
}

func TestPackageEndpointNotFound(t *testing.T) {
	harness := newHarness(t)

	response := send(t, harness.handler, http.MethodGet, "/v1/packages/nothing", "", "")
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}

	if details := decodeErrors(t, response.Body); len(details) == 0 {
		t.Error("404 carried no diagnostic")
	}
}

// The owners endpoint is three segments, and so is a scoped package. Adding the detail
// route must not have made one shadow the other.
func TestPackageAndOwnerRoutesCoexist(t *testing.T) {
	harness := newHarness(t)

	owners := send(t, harness.handler, http.MethodGet, "/v1/packages/serio/owners", "", "")
	if owners.Code != http.StatusOK {
		t.Fatalf("owners = %d, want 200: %s", owners.Code, owners.Body)
	}

	if !strings.Contains(owners.Body.String(), `"owners"`) {
		t.Errorf("the owners route answered a package detail: %s", owners.Body)
	}
}

func TestCatalogReportsAStoreFailureAsAFailure(t *testing.T) {
	harness := newHarness(t)
	harness.catalog.err = errors.New("connection refused")

	for _, path := range []string{"/v1/search?q=serio", "/v1/packages/serio"} {
		t.Run(path, func(t *testing.T) {
			response := send(t, harness.handler, http.MethodGet, path, "", "")
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", response.Code)
			}

			if details := decodeErrors(t, response.Body); strings.Contains(strings.Join(details, " "), "connection refused") {
				t.Errorf("the internal error leaked: %v", details)
			}
		})
	}
}
