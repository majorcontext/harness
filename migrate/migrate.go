// Package migrate converts the session journals of the engine to event logs
// in a harness.Store. It runs once, in the quiesced window of the cutover,
// and is the only code that reads an old format. It never changes or
// removes an old file.
package migrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/engine"
)

// Result is the outcome of one session. Err is nil for a converted or a
// skipped session.
type Result struct {
	Session string
	Skipped bool
	// Messages is the message count of the converted history.
	Messages int
	Err      error
}

// Failed returns the results that hold an error.
func Failed(results []Result) []Result {
	var out []Result
	for _, r := range results {
		if r.Err != nil {
			out = append(out, r)
		}
	}
	return out
}

// Dir converts each engine journal in dir to a log in st. It skips a
// session that st already holds, and reports each other session.
func Dir(ctx context.Context, dir string, st harness.Store) ([]Result, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if e.IsDir() || !ok || !engine.ValidSessionID(id) {
			continue
		}
		out = append(out, convert(ctx, st, id, func() (old, error) { return readJournal(dir, id) }))
	}
	return out, nil
}

// convert writes the session that read returns to st, unless st holds it.
func convert(ctx context.Context, st harness.Store, id string, read func() (old, error)) Result {
	r := Result{Session: id}
	head, err := st.Head(ctx, id)
	switch {
	case err != nil:
		r.Err = err
		return r
	case head > 0:
		r.Skipped = true
		return r
	}
	o, err := read()
	if err == nil {
		r.Messages, err = write(ctx, st, id, o)
	}
	if err != nil {
		r.Err = fmt.Errorf("migrate: session %s: %w", id, err)
	}
	return r
}

// write builds and checks the log of o, then stores its blobs and its
// records. The records go in one append, so a failed session has no log.
func write(ctx context.Context, st harness.Store, id string, o old) (int, error) {
	records, n, err := build(o)
	if err != nil {
		return 0, err
	}
	for _, key := range sortedKeys(o.blobs) {
		if err := st.PutBlob(ctx, id, key, bytes.NewReader(o.blobs[key])); err != nil {
			return 0, err
		}
	}
	return n, st.Append(ctx, id, 0, records...)
}

func toolResultDir(dir, id string) string { return filepath.Join(dir, "toolresults", id) }
