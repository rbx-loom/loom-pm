package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"
)

// RequestIDHeader is where the id of a request is echoed, so a client reporting a failure
// can quote the same id the log line carries.
const RequestIDHeader = "X-Request-Id"

type contextKey struct{}

// observed gives every request an id and logs how it ended.
//
// The id is minted here rather than read from the request: a client-supplied one would let
// anybody collide with, or forge, an entry in the registry's own logs.
func observed(handler http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set(RequestIDHeader, id)

		r = r.WithContext(context.WithValue(r.Context(), contextKey{}, id))
		recorder := &recordingWriter{ResponseWriter: w, status: http.StatusOK}

		started := time.Now()
		handler.ServeHTTP(recorder, r)

		logger.InfoContext(r.Context(), "served",
			"request", id,
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"bytes", recorder.written,
			"duration", time.Since(started))
	})
}

func requestIDOf(ctx context.Context) string {
	id, _ := ctx.Value(contextKey{}).(string)
	return id
}

func newRequestID() string {
	var raw [8]byte
	rand.Read(raw[:])

	return hex.EncodeToString(raw[:])
}

// recordingWriter remembers what a handler answered, which is the half of an access log
// the request itself does not carry.
type recordingWriter struct {
	http.ResponseWriter
	status  int
	written int64
}

func (w *recordingWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *recordingWriter) Write(content []byte) (int, error) {
	written, err := w.ResponseWriter.Write(content)
	w.written += int64(written)

	return written, err
}
