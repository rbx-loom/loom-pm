package usage

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"
)

type fakeSink struct {
	downloads []map[int64]int64
	tokens    [][][]byte
	err       error
}

func (f *fakeSink) AddDownloads(_ context.Context, counts map[int64]int64) error {
	f.downloads = append(f.downloads, counts)
	return f.err
}

func (f *fakeSink) TouchTokens(_ context.Context, hashes [][]byte) error {
	f.tokens = append(f.tokens, hashes)
	return f.err
}

func newTestRecorder(sink Sink) *Recorder {
	return NewRecorder(sink, time.Minute, slog.New(slog.DiscardHandler))
}

func TestFlushBatchesDownloadsPerVersion(t *testing.T) {
	sink := &fakeSink{}
	recorder := newTestRecorder(sink)

	recorder.Download(7)
	recorder.Download(7)
	recorder.Download(9)

	recorder.flush(context.Background())

	if len(sink.downloads) != 1 {
		t.Fatalf("the sink was written %d times, want 1", len(sink.downloads))
	}

	written := sink.downloads[0]
	if written[7] != 2 || written[9] != 1 {
		t.Errorf("counts = %v, want {7: 2, 9: 1}", written)
	}
}

// Repeats collapse, because last_used_at only holds the latest of them.
func TestFlushCollapsesRepeatedTokens(t *testing.T) {
	sink := &fakeSink{}
	recorder := newTestRecorder(sink)

	recorder.TokenUsed([]byte("first"))
	recorder.TokenUsed([]byte("first"))
	recorder.TokenUsed([]byte("second"))

	recorder.flush(context.Background())

	if len(sink.tokens) != 1 {
		t.Fatalf("the sink was written %d times, want 1", len(sink.tokens))
	}

	written := sink.tokens[0]
	if len(written) != 2 {
		t.Fatalf("wrote %d hashes, want 2", len(written))
	}

	for _, wanted := range [][]byte{[]byte("first"), []byte("second")} {
		if !slices.ContainsFunc(written, func(hash []byte) bool { return string(hash) == string(wanted) }) {
			t.Errorf("%q was not written", wanted)
		}
	}
}

func TestFlushWritesNothingWhenNothingHappened(t *testing.T) {
	sink := &fakeSink{}
	newTestRecorder(sink).flush(context.Background())

	if len(sink.downloads) != 0 || len(sink.tokens) != 0 {
		t.Error("an idle recorder wrote to the database")
	}
}

// A flush hands over what it took, so the next one does not write the same tallies again.
func TestFlushClearsWhatItWrote(t *testing.T) {
	sink := &fakeSink{}
	recorder := newTestRecorder(sink)

	recorder.Download(7)
	recorder.flush(context.Background())
	recorder.flush(context.Background())

	if len(sink.downloads) != 1 {
		t.Errorf("the sink was written %d times, want 1", len(sink.downloads))
	}
}

// A failed batch is dropped rather than retried: a lost statistic is cheaper than a
// backlog that grows for as long as the database is unwell.
func TestAFailedFlushDiscardsItsBatch(t *testing.T) {
	sink := &fakeSink{err: errors.New("the database is down")}
	recorder := newTestRecorder(sink)

	recorder.Download(7)
	recorder.flush(context.Background())

	sink.err = nil
	recorder.flush(context.Background())

	if len(sink.downloads) != 1 {
		t.Errorf("the sink was written %d times, want 1", len(sink.downloads))
	}
}

func TestRunFlushesWhatIsLeftWhenItStops(t *testing.T) {
	sink := &fakeSink{}
	recorder := NewRecorder(sink, time.Hour, slog.New(slog.DiscardHandler))

	recorder.Download(7)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		recorder.Run(ctx)
		close(stopped)
	}()

	cancel()
	<-stopped

	if len(sink.downloads) != 1 {
		t.Fatalf("the sink was written %d times, want 1", len(sink.downloads))
	}
	if sink.downloads[0][7] != 1 {
		t.Errorf("counts = %v, want {7: 1}", sink.downloads[0])
	}
}
