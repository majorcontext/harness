package httpsync

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/protocol"
)

// ImportSummary counts what ImportBatches applied.
type ImportSummary struct {
	Sessions int
	Records  int
	Blobs    int
}

// ImportBatches reads one JSON SyncBatch for each line of r and applies each
// to st with ApplySync, in order. It stops at the first line that is not a
// batch, and at the first batch that ApplySync rejects or that does not extend
// its session: a batch that does not start at the record after the head of
// st is an error, because an import has no sender to resend.
func ImportBatches(ctx context.Context, st harness.Store, r io.Reader) (ImportSummary, error) {
	var sum ImportSummary
	seen := map[string]bool{}
	br := bufio.NewReader(r)
	for line := 1; ; line++ {
		data, err := br.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return sum, err
		}
		if len(trimEOL(data)) > 0 {
			var b protocol.SyncBatch
			if jerr := json.Unmarshal(data, &b); jerr != nil {
				return sum, fmt.Errorf("line %d: not a sync batch: %w", line, jerr)
			}
			if len(b.Records) == 0 {
				return sum, fmt.Errorf("line %d: session %s: batch has no records", line, b.Session)
			}
			head, herr := st.Head(ctx, b.Session)
			if herr != nil {
				return sum, fmt.Errorf("line %d: session %s: %w", line, b.Session, herr)
			}
			if b.FromSeq != head+1 {
				return sum, fmt.Errorf("line %d: session %s: batch starts at %d, the log ends at %d", line, b.Session, b.FromSeq, head)
			}
			if _, aerr := ApplySync(ctx, st, b); aerr != nil {
				return sum, fmt.Errorf("line %d: session %s: %w", line, b.Session, aerr)
			}
			if !seen[b.Session] {
				seen[b.Session] = true
				sum.Sessions++
			}
			sum.Records += len(b.Records)
			sum.Blobs += len(b.Blobs)
		}
		if err != nil {
			return sum, nil
		}
	}
}

func trimEOL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}
