package api

import (
	"net/http"
)

// publicReads are the routes any page may read: everything unauthenticated, which is
// everything a resolver or a browser needs to look at packages.
//
// Nothing that takes a token is here. A token-authenticated endpoint opened to any origin
// turns a leaked credential into a request the browser will make on somebody else's behalf,
// and none of these endpoints has a reason to be called from a page anyway.
var publicReads = map[string]bool{
	"index":    true,
	"download": true,
	"search":   true,
	"package":  true,
	"owners":   true,
}

// allowedRequestHeaders is what a preflight permits. If-None-Match is the one that matters:
// it is not safelisted, so without this a browser silently drops the revalidation and
// refetches the whole index document every time.
const allowedRequestHeaders = "If-None-Match, Accept"

// crossOrigin answers browser requests for the public read API.
//
// The allowance is unconditional because what it protects is already public: these routes
// answer the same bytes to anybody who asks, with or without a browser. Listing origins
// would suggest otherwise without making anything safer.
func crossOrigin(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// a request without an Origin is not a browser's, and gains nothing from any of this
		if r.Header.Get("Origin") == "" || !readable(r) {
			handler.ServeHTTP(w, r)
			return
		}

		header := w.Header()
		header.Set("Access-Control-Allow-Origin", "*")

		// Vary, because a cache in front of this must not hand the header to a request
		// that did not carry an Origin, nor withhold it from one that did
		header.Add("Vary", "Origin")

		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			header.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
			header.Set("Access-Control-Allow-Headers", allowedRequestHeaders)
			header.Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// a script cannot read a response header it was not given, and the ETag is the one
		// it has to echo back to revalidate
		header.Set("Access-Control-Expose-Headers", "ETag, Content-Length")

		handler.ServeHTTP(w, r)
	})
}

// readable answers whether a request is one of the public reads.
//
// The method is half the question: the owners of a package are public to read and not to
// change, and both are the same path.
func readable(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		return false
	}

	return publicReads[route(r.URL.Path)]
}
