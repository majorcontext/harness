package main

import (
	"context"
	"log/slog"
	"runtime/metrics"
	"time"
)

// A stop-the-world garbage collection pause stops every goroutine at once:
// the process answers nothing, logs nothing, and looks identical from the
// outside to a wedged handler or a blocking syscall. gcWatcher makes the
// garbage-collection case say so, which by elimination also narrows the
// other two.

// gcPausesMetric is the runtime's per-pause histogram. It is read through
// runtime/metrics rather than runtime.ReadMemStats: ReadMemStats itself
// stops the world, so sampling it would add the kind of pause this watcher
// exists to find.
const gcPausesMetric = "/gc/pauses:seconds"

// gcPauseThreshold is the cutoff for the warn below. An ordinary pause is
// well under a millisecond, so 200ms is already a pause a caller can feel.
const gcPauseThreshold = 200 * time.Millisecond

// gcSampleInterval is how often the watcher re-reads the histogram. The
// counts are cumulative, so a longer interval loses no pause; it only
// delays the report.
const gcSampleInterval = 5 * time.Second

// longGCPauseMsg is the warn line's message.
const longGCPauseMsg = "long gc pause"

// gcWatcher samples the runtime's pause histogram and warns about pauses
// past its threshold. read is the histogram source, replaced in tests.
type gcWatcher struct {
	logger    *slog.Logger
	threshold time.Duration
	read      func() *metrics.Float64Histogram

	prev []uint64
}

func newGCWatcher(logger *slog.Logger) *gcWatcher {
	return &gcWatcher{logger: logger, threshold: gcPauseThreshold, read: readGCPauses}
}

// run samples until ctx ends.
func (w *gcWatcher) run(ctx context.Context) {
	ticker := time.NewTicker(gcSampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.sample()
		}
	}
}

// sample reads the histogram once and warns about any pause past the
// threshold that the previous sample did not already cover.
func (w *gcWatcher) sample() {
	h := w.read()
	if h == nil {
		return
	}
	found := newLongPauses(h, w.prev, w.threshold)
	w.prev = append(w.prev[:0], h.Counts...)
	if found.count == 0 {
		return
	}
	w.logger.Warn(longGCPauseMsg,
		"pauses", found.count,
		"longest_pause_ms", found.longest.Milliseconds(),
		"threshold_ms", w.threshold.Milliseconds(),
		"window_ms", gcSampleInterval.Milliseconds(),
	)
}

// longPauses is what one sample found: how many pauses past the threshold
// are new, and a lower bound on the longest of them.
type longPauses struct {
	count   uint64
	longest time.Duration
}

// newLongPauses diffs a pause histogram against the previous sample's
// counts and reports the new pauses at or past threshold.
//
// longest is the LOWER bound of the highest bucket that gained a pause: a
// histogram records a range, so this is the largest value the data proves,
// never a guess at the real pause.
//
// A nil prev (the first sample) reports nothing — the counts are
// cumulative for the whole process life, so a first sample would warn at
// startup about pauses that already happened. A prev of a different length
// also reports nothing, since bucket-by-bucket subtraction across two
// different layouts would invent pauses.
func newLongPauses(h *metrics.Float64Histogram, prev []uint64, threshold time.Duration) longPauses {
	var found longPauses
	if prev == nil || len(prev) != len(h.Counts) {
		return found
	}
	cutoff := threshold.Seconds()
	for i, count := range h.Counts {
		if count <= prev[i] {
			continue
		}
		// Buckets[i] is bucket i's lower bound; a bucket qualifies only
		// when its whole range is at or past the threshold, so a pause
		// under the threshold can never be reported as one past it.
		lower := h.Buckets[i]
		if lower < cutoff {
			continue
		}
		found.count += count - prev[i]
		if d := time.Duration(lower * float64(time.Second)); d > found.longest {
			found.longest = d
		}
	}
	return found
}

// readGCPauses reads the runtime's pause histogram, or nil when this
// runtime does not publish it.
func readGCPauses() *metrics.Float64Histogram {
	sample := []metrics.Sample{{Name: gcPausesMetric}}
	metrics.Read(sample)
	if sample[0].Value.Kind() != metrics.KindFloat64Histogram {
		return nil
	}
	return sample[0].Value.Float64Histogram()
}
