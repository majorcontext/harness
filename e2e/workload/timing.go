//go:build e2e

package workload

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// timingRun is one measured phase of a dev workload (row 2's pnpm install /
// build / test triad), recorded so a later run against a different backend
// (Modal today, GKE/gVisor once deployed) can be diffed against it.
type timingRun struct {
	Phase      string        `json:"phase"`
	DurationMS int64         `json:"duration_ms"`
	Duration   time.Duration `json:"-"`
}

// timingArtifact is the full JSON document row 2 writes: one backend's
// timing for the whole dev-workload triad, tagged with enough context
// (backend, image, timestamp) that two artifacts from different runs can be
// told apart and diffed.
type timingArtifact struct {
	Backend   string      `json:"backend"`
	Image     string      `json:"image"`
	Timestamp time.Time   `json:"timestamp"`
	Runs      []timingRun `json:"runs"`
}

// timingRecorder accumulates timed phases for one run, then writes them to
// a JSON artifact for later GKE-vs-Modal comparison.
type timingRecorder struct {
	backend string
	image   string
	runs    []timingRun
}

func newTimingRecorder(backend, image string) *timingRecorder {
	return &timingRecorder{backend: backend, image: image}
}

// time runs fn, records its wall-clock duration under name, and returns
// fn's own error unchanged (a failed phase is still recorded, so a partial
// artifact still shows how far the run got before failing).
func (r *timingRecorder) time(name string, fn func() error) error {
	start := time.Now()
	err := fn()
	d := time.Since(start)
	r.runs = append(r.runs, timingRun{Phase: name, DurationMS: d.Milliseconds(), Duration: d})
	return err
}

// writeArtifact writes the recorded runs as JSON to path, creating parent
// directories as needed.
func (r *timingRecorder) writeArtifact(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	artifact := timingArtifact{
		Backend:   r.backend,
		Image:     r.image,
		Timestamp: time.Now().UTC(),
		Runs:      r.runs,
	}
	b, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// summary formats a one-line-per-phase human summary for test output.
func (r *timingRecorder) summary() string {
	s := fmt.Sprintf("timing for backend=%s image=%s:\n", r.backend, r.image)
	for _, run := range r.runs {
		s += fmt.Sprintf("  %-16s %v\n", run.Phase, run.Duration)
	}
	return s
}

// defaultTimingArtifactPath is where row 2 writes its JSON artifact unless
// TIMING_ARTIFACT_PATH overrides it.
func defaultTimingArtifactPath() string {
	if v := os.Getenv("TIMING_ARTIFACT_PATH"); v != "" {
		return v
	}
	return filepath.Join("testdata", fmt.Sprintf("timing-%d.json", time.Now().UnixNano()))
}
