package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func withOrigin(t *testing.T, h *harness, method, target string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(method, target, nil)
	request.Header.Set("Origin", "https://rbx-loom.github.io")

	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, request)
	return recorder
}

// Everything a browser may read is public and unauthenticated, so it is readable from
// anywhere: the website, the playground, somebody's dashboard.
func TestCORSAllowsPublicReads(t *testing.T) {
	harness := newHarness(t)

	paths := []string{
		"/v1/index/serio",
		"/v1/packages/serio/1.2.0/download",
		"/v1/search?q=serio",
		"/v1/packages/serio",
		"/v1/packages/serio/owners",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			response := withOrigin(t, harness, http.MethodGet, path)

			if got := response.Header().Get("Access-Control-Allow-Origin"); got != "*" {
				t.Errorf("Allow-Origin = %q, want *", got)
			}
		})
	}
}

// The index is revalidated with If-None-Match, which is not a safelisted request header, so
// a browser will not send it unless the preflight says it may.
func TestCORSAllowsRevalidation(t *testing.T) {
	harness := newHarness(t)

	request := httptest.NewRequest(http.MethodOptions, "/v1/index/serio", nil)
	request.Header.Set("Origin", "https://rbx-loom.github.io")
	request.Header.Set("Access-Control-Request-Method", "GET")
	request.Header.Set("Access-Control-Request-Headers", "if-none-match")

	recorder := httptest.NewRecorder()
	harness.handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("preflight = %d, want 204", recorder.Code)
	}

	if got := recorder.Header().Get("Access-Control-Allow-Headers"); got == "" {
		t.Error("the preflight allowed no headers, so If-None-Match will not be sent")
	}

	// and the tag itself has to be readable, or a client cannot send it back
	response := withOrigin(t, harness, http.MethodGet, "/v1/index/serio")
	if got := response.Header().Get("Access-Control-Expose-Headers"); got == "" {
		t.Error("no exposed headers, so a script cannot read the ETag it must echo")
	}
}

// A token-authenticated endpoint is not a public read. Letting any page call one turns a
// leaked token into a cross-site request the browser makes on its own.
func TestCORSRefusesWriteEndpoints(t *testing.T) {
	harness := newHarness(t)

	cases := []struct{ method, target string }{
		{http.MethodPost, "/v1/publish"},
		{http.MethodPut, "/v1/packages/serio/1.2.0/yank"},
		{http.MethodDelete, "/v1/packages/serio/1.2.0/yank"},
		{http.MethodPut, "/v1/packages/serio/owners"},
		{http.MethodGet, "/v1/me/tokens"},
		{http.MethodPost, "/v1/me/tokens"},
		{http.MethodGet, "/v1/auth/github"},
	}

	for _, testCase := range cases {
		t.Run(testCase.method+" "+testCase.target, func(t *testing.T) {
			response := withOrigin(t, harness, testCase.method, testCase.target)

			if got := response.Header().Get("Access-Control-Allow-Origin"); got != "" {
				t.Errorf("Allow-Origin = %q, want none on an authenticated endpoint", got)
			}
		})
	}
}

// A preflight for a write is refused rather than answered, so the browser never sends the
// request it was asking about.
func TestCORSRefusesAWritePreflight(t *testing.T) {
	harness := newHarness(t)

	request := httptest.NewRequest(http.MethodOptions, "/v1/publish", nil)
	request.Header.Set("Origin", "https://rbx-loom.github.io")
	request.Header.Set("Access-Control-Request-Method", "POST")

	recorder := httptest.NewRecorder()
	harness.handler.ServeHTTP(recorder, request)

	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q, want the preflight refused", got)
	}
}

// Nothing changes for a request that is not a browser's.
func TestCORSIsAbsentWithoutAnOrigin(t *testing.T) {
	harness := newHarness(t)

	response := get(t, harness.handler, "/v1/index/serio", nil)
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q, want none when no Origin was sent", got)
	}

	if response.Code != http.StatusOK {
		t.Errorf("status = %d, want the request served as before", response.Code)
	}
}

// The metrics endpoint may carry a token, and is nobody's business from a page.
func TestCORSRefusesMetrics(t *testing.T) {
	harness := newHarness(t)

	response := withOrigin(t, harness, http.MethodGet, "/metrics")
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q, want none on /metrics", got)
	}
}
