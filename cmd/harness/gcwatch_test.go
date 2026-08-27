package main

import (
	"bytes"
	"log/slog"
	"math"
	"runtime/metrics"
	"strings"
	"testing"
	"time"
)

// histogram builds a Float64Histogram over the given bucket boundaries.
func histogram(bounds []float64, counts []uint64) *metrics.Float64Histogram {
	return &metrics.Float64Histogram{Buckets: bounds, Counts: counts}
}

// pauseBounds are bucket boundaries in seconds around the 200ms threshold:
// 0-1ms, 1-100ms, 100-200ms, 200-500ms, 500ms-1s, 1s-infinity.
var pauseBounds = []float64{0, 0.001, 0.1, 0.2, 0.5, 1, math.Inf(1)}

func TestNewLongPauses_CountsOnlyNewPausesPastThreshold(t *testing.T) {
	prev := []uint64{10, 5, 2, 0, 0, 0}
	// Two new pauses: one at 100-200ms (under the threshold) and one at
	// 500ms-1s (over it).
	cur := []uint64{10, 5, 3, 0, 1, 0}

	got := newLongPauses(histogram(pauseBounds, cur), prev, 200*time.Millisecond)
	if got.count != 1 {
		t.Errorf("count = %d, want 1 (the sub-threshold pause must not count)", got.count)
	}
	if got.longest != 500*time.Millisecond {
		t.Errorf("longest = %v, want 500ms (the bucket's lower bound)", got.longest)
	}
}

func TestNewLongPauses_IgnoresPausesAlreadyReported(t *testing.T) {
	prev := []uint64{0, 0, 0, 4, 1, 0}
	cur := []uint64{0, 0, 0, 4, 1, 0}

	if got := newLongPauses(histogram(pauseBounds, cur), prev, 200*time.Millisecond); got.count != 0 {
		t.Errorf("count = %d, want 0: a pause counted in an earlier sample must never be re-reported", got.count)
	}
}

func TestNewLongPauses_FirstSampleReportsNothing(t *testing.T) {
	cur := []uint64{3, 9, 1, 2, 0, 0}

	// A nil previous sample is the process's first read. Its counts are
	// cumulative for the whole process life, so reporting them would warn
	// at startup about pauses that already happened.
	if got := newLongPauses(histogram(pauseBounds, cur), nil, 200*time.Millisecond); got.count != 0 {
		t.Errorf("count = %d, want 0 on the first sample", got.count)
	}
}

func TestNewLongPauses_ToleratesBucketLayoutChange(t *testing.T) {
	prev := []uint64{1, 1}
	cur := []uint64{0, 0, 0, 5, 0, 0}

	// A previous sample of a different length cannot be compared bucket by
	// bucket. Reporting nothing is the safe answer; a wrong diff would
	// invent pauses.
	if got := newLongPauses(histogram(pauseBounds, cur), prev, 200*time.Millisecond); got.count != 0 {
		t.Errorf("count = %d, want 0 when the bucket layout changed", got.count)
	}
}

func TestNewLongPauses_CountsEveryBucketPastThreshold(t *testing.T) {
	prev := []uint64{0, 0, 0, 0, 0, 0}
	cur := []uint64{0, 0, 0, 2, 1, 3}

	got := newLongPauses(histogram(pauseBounds, cur), prev, 200*time.Millisecond)
	if got.count != 6 {
		t.Errorf("count = %d, want 6", got.count)
	}
	if got.longest != 1*time.Second {
		t.Errorf("longest = %v, want 1s", got.longest)
	}
}

// TestGCWatcher_WarnsOnLongPause proves the sampler turns a long pause into
// one warn naming how many pauses landed and how long the longest was.
func TestGCWatcher_WarnsOnLongPause(t *testing.T) {
	var logBuf bytes.Buffer
	w := &gcWatcher{
		logger:    slog.New(slog.NewTextHandler(&logBuf, nil)),
		threshold: 200 * time.Millisecond,
		read: func() *metrics.Float64Histogram {
			return histogram(pauseBounds, []uint64{0, 0, 0, 0, 2, 0})
		},
	}

	w.sample() // first sample: baseline only, no warn
	if strings.Contains(logBuf.String(), longGCPauseMsg) {
		t.Fatalf("the first sample warned: %s", logBuf.String())
	}

	w.read = func() *metrics.Float64Histogram {
		return histogram(pauseBounds, []uint64{0, 0, 0, 0, 2, 1})
	}
	w.sample()

	logged := logBuf.String()
	if !strings.Contains(logged, longGCPauseMsg) {
		t.Fatalf("a new long pause did not warn: %s", logged)
	}
	for _, want := range []string{"level=WARN", "pauses=1", "longest_pause_ms=1000", "threshold_ms=200"} {
		if !strings.Contains(logged, want) {
			t.Errorf("warn line is missing %q: %s", want, logged)
		}
	}
}

// TestGCWatcher_QuietWithoutLongPauses proves ordinary garbage collection
// logs nothing at all.
func TestGCWatcher_QuietWithoutLongPauses(t *testing.T) {
	var logBuf bytes.Buffer
	counts := []uint64{5, 4, 0, 0, 0, 0}
	w := &gcWatcher{
		logger:    slog.New(slog.NewTextHandler(&logBuf, nil)),
		threshold: 200 * time.Millisecond,
		read:      func() *metrics.Float64Histogram { return histogram(pauseBounds, counts) },
	}

	w.sample()
	counts = []uint64{9, 12, 3, 0, 0, 0} // more pauses, all short
	w.sample()

	if logBuf.Len() != 0 {
		t.Errorf("short pauses produced output: %s", logBuf.String())
	}
}

// TestReadGCPauses_ReadsTheRuntime proves the production reader returns the
// real runtime histogram, so the sampler is wired to something live rather
// than to a metric name the runtime does not publish.
func TestReadGCPauses_ReadsTheRuntime(t *testing.T) {
	h := readGCPauses()
	if h == nil {
		t.Fatalf("readGCPauses returned nil: %q is not published by this runtime", gcPausesMetric)
	}
	if len(h.Counts)+1 != len(h.Buckets) {
		t.Errorf("histogram shape is wrong: %d counts, %d bucket bounds", len(h.Counts), len(h.Buckets))
	}
}
