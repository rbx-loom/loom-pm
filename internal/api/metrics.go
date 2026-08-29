package api

import (
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rbx-loom/loom-pm/internal/auth"
)

// buckets are the latency boundaries the histogram reports, in seconds. They stop at ten:
// a request slower than that is an outage rather than a percentile.
var buckets = [...]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// metrics is a Prometheus exposition small enough not to want a library.
//
// Every label here is drawn from a fixed set. Nothing derived from a path — a package
// name, a version — is ever a label: those are unbounded, and one series per package is
// how a registry's metrics take down the thing collecting them.
type metrics struct {
	inFlight atomic.Int64

	mu        sync.Mutex
	requests  map[requestLabels]int64
	durations map[string]*histogram
	cache     map[string]int64
}

type requestLabels struct {
	route  string
	status int
}

type histogram struct {
	counts [len(buckets) + 1]int64
	sum    float64
	total  int64
}

func newMetrics() *metrics {
	return &metrics{
		requests:  map[requestLabels]int64{},
		durations: map[string]*histogram{},
		cache:     map[string]int64{},
	}
}

func (m *metrics) observe(route string, status int, took time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.requests[requestLabels{route: route, status: status}]++

	held, ok := m.durations[route]
	if !ok {
		held = &histogram{}
		m.durations[route] = held
	}

	seconds := took.Seconds()
	held.sum += seconds
	held.total++

	held.counts[sort.SearchFloat64s(buckets[:], seconds)]++
}

func (m *metrics) indexCache(hit bool) {
	result := "miss"
	if hit {
		result = "hit"
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.cache[result]++
}

func (m *metrics) write(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()

	fmt.Fprint(w, "# HELP loom_http_requests_total Requests served, by route and status.\n")
	fmt.Fprint(w, "# TYPE loom_http_requests_total counter\n")

	for _, labels := range sortedRequestLabels(m.requests) {
		fmt.Fprintf(w, "loom_http_requests_total{route=%q,status=%q} %d\n",
			labels.route, strconv.Itoa(labels.status), m.requests[labels])
	}

	fmt.Fprint(w, "# HELP loom_http_request_duration_seconds How long requests took, by route.\n")
	fmt.Fprint(w, "# TYPE loom_http_request_duration_seconds histogram\n")

	for _, route := range sortedKeys(m.durations) {
		held := m.durations[route]

		// cumulative, because that is how a histogram is read
		var running int64
		for index, boundary := range buckets {
			running += held.counts[index]
			fmt.Fprintf(w, "loom_http_request_duration_seconds_bucket{route=%q,le=%q} %d\n",
				route, strconv.FormatFloat(boundary, 'g', -1, 64), running)
		}

		running += held.counts[len(buckets)]
		fmt.Fprintf(w, "loom_http_request_duration_seconds_bucket{route=%q,le=\"+Inf\"} %d\n", route, running)
		fmt.Fprintf(w, "loom_http_request_duration_seconds_sum{route=%q} %s\n",
			route, strconv.FormatFloat(held.sum, 'g', -1, 64))
		fmt.Fprintf(w, "loom_http_request_duration_seconds_count{route=%q} %d\n", route, held.total)
	}

	fmt.Fprint(w, "# HELP loom_index_cache_total Index documents served from the cache, or rebuilt.\n")
	fmt.Fprint(w, "# TYPE loom_index_cache_total counter\n")

	for _, result := range sortedKeys(m.cache) {
		fmt.Fprintf(w, "loom_index_cache_total{result=%q} %d\n", result, m.cache[result])
	}

	fmt.Fprint(w, "# HELP loom_http_requests_in_flight Requests being served right now.\n")
	fmt.Fprint(w, "# TYPE loom_http_requests_in_flight gauge\n")
	fmt.Fprintf(w, "loom_http_requests_in_flight %d\n", m.inFlight.Load())
}

func (a *API) serveMetrics(w http.ResponseWriter, r *http.Request) {
	if a.MetricsToken != "" {
		presented, _ := auth.BearerToken(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare([]byte(presented), []byte(a.MetricsToken)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="loom"`)
			a.fail(w, r, http.StatusUnauthorized, "this needs the metrics token.")
			return
		}
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	a.metrics.write(w)
}

// route names a request with a label from a fixed set, so the exposition's cardinality is
// the number of endpoints rather than the number of packages.
func route(path string) string {
	switch {
	case path == "/healthz":
		return "health"
	case path == "/metrics":
		return "metrics"
	case path == "/v1/publish":
		return "publish"
	case path == "/v1/search":
		return "search"
	case strings.HasPrefix(path, "/v1/index/"):
		return "index"
	case strings.HasPrefix(path, "/v1/me/tokens"):
		return "tokens"
	case strings.HasPrefix(path, "/v1/auth/"):
		return "auth"
	case strings.HasPrefix(path, "/v1/packages/"):
		switch {
		case strings.HasSuffix(path, "/download"):
			return "download"
		case strings.HasSuffix(path, "/yank"):
			return "yank"
		case strings.HasSuffix(path, "/owners"):
			return "owners"
		default:
			return "package"
		}
	}

	return "other"
}

func sortedKeys[V any](held map[string]V) []string {
	keys := make([]string, 0, len(held))
	for key := range held {
		keys = append(keys, key)
	}

	sort.Strings(keys)
	return keys
}

func sortedRequestLabels(held map[requestLabels]int64) []requestLabels {
	labels := make([]requestLabels, 0, len(held))
	for key := range held {
		labels = append(labels, key)
	}

	sort.Slice(labels, func(left, right int) bool {
		if labels[left].route != labels[right].route {
			return labels[left].route < labels[right].route
		}
		return labels[left].status < labels[right].status
	})

	return labels
}
