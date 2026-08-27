// Package usage counts what the registry is used for, away from the request that caused it.
//
// A download and a token presentation are both worth recording and neither is worth a
// write per request: a popular package would otherwise turn a cacheable, read-only route
// into one UPDATE per client, on the same pool publishing depends on. Counts are held in
// memory and flushed in batches, so a crash costs at most one interval of tallies — which
// is the right trade for a statistic and would be the wrong one for a publication.
package usage

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Sink is where a flush lands.
type Sink interface {
	AddDownloads(ctx context.Context, counts map[int64]int64) error
	TouchTokens(ctx context.Context, hashes [][]byte) error
}

type Recorder struct {
	sink     Sink
	interval time.Duration
	logger   *slog.Logger

	mu        sync.Mutex
	downloads map[int64]int64
	tokens    map[string]struct{}
}

func NewRecorder(sink Sink, interval time.Duration, logger *slog.Logger) *Recorder {
	return &Recorder{
		sink:      sink,
		interval:  interval,
		logger:    logger,
		downloads: map[int64]int64{},
		tokens:    map[string]struct{}{},
	}
}

func (r *Recorder) Download(versionID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.downloads[versionID]++
}

// TokenUsed records that a token was presented. Repeats within one interval collapse,
// because last_used_at only ever holds the latest of them.
func (r *Recorder) TokenUsed(hash []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.tokens[string(hash)] = struct{}{}
}

// Run flushes on every interval until ctx is done, then flushes what is left. It blocks,
// so it belongs in its own goroutine.
func (r *Recorder) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			r.flush(ctx)
		case <-ctx.Done():
			// the context that ended the server cannot be the one that writes the last
			// tallies out
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.interval)
			defer cancel()

			r.flush(final)
			return
		}
	}
}

func (r *Recorder) flush(ctx context.Context) {
	downloads, tokens := r.take()

	if len(downloads) > 0 {
		if err := r.sink.AddDownloads(ctx, downloads); err != nil {
			r.logger.ErrorContext(ctx, "recording downloads", "error", err, "versions", len(downloads))
		}
	}

	if len(tokens) > 0 {
		if err := r.sink.TouchTokens(ctx, tokens); err != nil {
			r.logger.ErrorContext(ctx, "recording token use", "error", err, "tokens", len(tokens))
		}
	}
}

// take hands over what has accumulated and resets the tallies, so a slow flush does not
// hold the lock the request path takes. A failed flush discards its batch rather than
// retrying it: a lost count is cheaper than an unbounded backlog.
func (r *Recorder) take() (map[int64]int64, [][]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	downloads, tokens := r.downloads, make([][]byte, 0, len(r.tokens))
	for hash := range r.tokens {
		tokens = append(tokens, []byte(hash))
	}

	r.downloads = map[int64]int64{}
	r.tokens = map[string]struct{}{}

	return downloads, tokens
}
