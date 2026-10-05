package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// stopReportFile is the name of the stop report in the session directory.
const stopReportFile = "serve-stop.json"

// stopReport tells the supervisor of serve how it stopped. Stop is handoff
// when every session released its ownership within the close deadline, else
// crashed. Sync is synced when a Sync holds every record, else unsynced.
type stopReport struct {
	Stop string `json:"stop"`
	Sync string `json:"sync"`
}

// removeStopReport deletes the report of an earlier run, so that a run that
// ends without a report reads as crashed.
func removeStopReport(dir string) error {
	if err := os.Remove(filepath.Join(dir, stopReportFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// writeStopReport writes the one-line report for a close that returned
// closeErr. replicated says that a Sync is set, the start caught up every
// stored session, and no Sync rejected a batch for good, so a release that
// returned nil left the Sync with every record.
func writeStopReport(dir string, closeErr error, replicated bool) error {
	r := stopReport{Stop: "handoff", Sync: "synced"}
	if closeErr != nil {
		r.Stop = "crashed"
	}
	if closeErr != nil || !replicated {
		r.Sync = "unsynced"
	}
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, stopReportFile+".tmp")
	if err := os.WriteFile(tmp, append(line, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, stopReportFile))
}
