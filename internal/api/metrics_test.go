package api

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func scrape(t *testing.T, h *harness, token string) string {
	t.Helper()

	response := send(t, h.handler, http.MethodGet, "/metrics", token, "")
	if response.Code != http.StatusOK {
		t.Fatalf("scraping = %d, want 200: %s", response.Code, response.Body)
	}

	return response.Body.String()
}

func TestMetricsCountsRequests(t *testing.T) {
	harness := newHarness(t)

	for range 3 {
		get(t, harness.handler, "/v1/index/serio", nil)
	}
	get(t, harness.handler, "/v1/index/nothing", nil)

	scraped := scrape(t, harness, "")

	if !strings.Contains(scraped, `loom_http_requests_total{route="index",status="200"} 3`) {
		t.Errorf("the served requests were not counted:\n%s", scraped)
	}

	if !strings.Contains(scraped, `loom_http_requests_total{route="index",status="404"} 1`) {
		t.Errorf("the 404 was not counted:\n%s", scraped)
	}
}

// A label taken from the path would make one time series per package, which is how a
// registry's own metrics take down its monitoring.
func TestMetricsLabelsAreBounded(t *testing.T) {
	harness := newHarness(t)

	get(t, harness.handler, "/v1/index/serio", nil)
	get(t, harness.handler, "/v1/packages/serio/1.2.0/download", nil)
	send(t, harness.handler, http.MethodGet, "/v1/packages/serio/owners", "", "")

	scraped := scrape(t, harness, "")

	for _, unbounded := range []string{"serio", "1.2.0"} {
		if strings.Contains(scraped, unbounded) {
			t.Errorf("the exposition names %q, which is unbounded:\n%s", unbounded, scraped)
		}
	}

	for _, route := range []string{`route="index"`, `route="download"`, `route="owners"`} {
		if !strings.Contains(scraped, route) {
			t.Errorf("no series for %s:\n%s", route, scraped)
		}
	}
}

func TestMetricsExposesDurations(t *testing.T) {
	harness := newHarness(t)
	get(t, harness.handler, "/v1/index/serio", nil)

	scraped := scrape(t, harness, "")

	for _, line := range []string{
		`# TYPE loom_http_request_duration_seconds histogram`,
		`loom_http_request_duration_seconds_bucket{route="index",le="+Inf"} 1`,
		`loom_http_request_duration_seconds_count{route="index"} 1`,
		`loom_http_request_duration_seconds_sum{route="index"}`,
	} {
		if !strings.Contains(scraped, line) {
			t.Errorf("missing %q:\n%s", line, scraped)
		}
	}
}

// Prometheus reads a histogram's buckets as cumulative, so each must be at least the one
// below it or the quantiles come out nonsense.
func TestMetricsBucketsAreCumulative(t *testing.T) {
	harness := newHarness(t)

	for range 5 {
		get(t, harness.handler, "/v1/index/serio", nil)
	}

	var last int
	for _, line := range strings.Split(scrape(t, harness, ""), "\n") {
		if !strings.HasPrefix(line, "loom_http_request_duration_seconds_bucket{route=\"index\"") {
			continue
		}

		var count int
		if _, err := fmtSscanLast(line, &count); err != nil {
			t.Fatalf("reading %q: %v", line, err)
		}

		if count < last {
			t.Errorf("bucket counts went backwards at %q", line)
		}
		last = count
	}

	if last != 5 {
		t.Errorf("the final bucket holds %d, want every one of the 5 requests", last)
	}
}

func TestMetricsCountsTheIndexCache(t *testing.T) {
	harness := newHarness(t)

	get(t, harness.handler, "/v1/index/serio", nil)
	get(t, harness.handler, "/v1/index/serio", nil)

	scraped := scrape(t, harness, "")

	if !strings.Contains(scraped, `loom_index_cache_total{result="miss"} 1`) {
		t.Errorf("the first read was not a miss:\n%s", scraped)
	}

	if !strings.Contains(scraped, `loom_index_cache_total{result="hit"} 1`) {
		t.Errorf("the second read was not a hit:\n%s", scraped)
	}
}

func TestMetricsInFlightSettlesAtZero(t *testing.T) {
	harness := newHarness(t)
	get(t, harness.handler, "/v1/index/serio", nil)

	scraped := scrape(t, harness, "")

	// the scrape itself is the only request in flight while it is being written
	if !strings.Contains(scraped, "loom_http_requests_in_flight 1") {
		t.Errorf("in-flight did not settle:\n%s", scraped)
	}
}

func TestMetricsIsProtectedWhenATokenIsSet(t *testing.T) {
	harness := newHarnessWithMetricsToken(t, "s3cret")

	response := send(t, harness.handler, http.MethodGet, "/metrics", "", "")
	if response.Code != http.StatusUnauthorized {
		t.Errorf("an unauthenticated scrape = %d, want 401", response.Code)
	}

	response = send(t, harness.handler, http.MethodGet, "/metrics", "wrong", "")
	if response.Code != http.StatusUnauthorized {
		t.Errorf("a scrape with the wrong token = %d, want 401", response.Code)
	}

	if scraped := scrape(t, harness, "s3cret"); !strings.Contains(scraped, "loom_http_requests_total") {
		t.Errorf("an authenticated scrape returned nothing useful:\n%s", scraped)
	}
}

func TestMetricsContentType(t *testing.T) {
	harness := newHarness(t)

	response := send(t, harness.handler, http.MethodGet, "/metrics", "", "")
	if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type = %q, want the Prometheus text exposition", got)
	}
}

// fmtSscanLast reads the trailing integer of an exposition line.
func fmtSscanLast(line string, into *int) (int, error) {
	fields := strings.Fields(line)

	value, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil {
		return 0, err
	}

	*into = value
	return 1, nil
}
