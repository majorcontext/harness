package session

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
)

// stateChunkBytes bounds one chunk of backend state. A chunk with its record
// then fits a SyncBatch far below its body cap. An entry over the bound is a
// chunk of its own.
const stateChunkBytes = 4 << 20

type stateBlob struct {
	key  string
	data []byte
}

// statePlan is what one save writes: the chunk blobs, then the records that
// name them.
type statePlan struct {
	blobs  []stateBlob
	events []eventlog.Event
}

// planSave plans the save of s over the chain that the log holds, or over
// no chain when exists is false. Chunk keys are backend, fenced, and an
// index that starts at next. The save adds the entries of s that follow the
// chain, and starts a new chain when the chain is not a prefix of them.
func planSave(backend string, fenced uint64, next int, chain eventlog.BackendChain, exists bool, s turn.Snapshot) (statePlan, error) {
	var head bytes.Buffer
	if err := json.Compact(&head, s.Head); err != nil || head.Len() == 0 {
		return statePlan{}, errors.New("session: backend state needs a JSON head")
	}
	all, offs, err := encodeEntries(s.Entries)
	if err != nil {
		return statePlan{}, err
	}
	count := len(s.Entries)
	h := sha256.New()
	n := chain.Entries
	keep := exists && chain.Legacy == "" && n <= count
	if keep {
		h.Write(all[:offs[n]])
		keep = n == 0 || hex.EncodeToString(h.Sum(nil)) == chain.Sum
	}
	restart := !keep
	if restart {
		h.Reset()
		n = 0
	}
	var plan statePlan
	for i := n; i < count; {
		j := i + 1
		for j < count && offs[j+1]-offs[i] <= stateChunkBytes {
			j++
		}
		h.Write(all[offs[i]:offs[j]])
		key := fmt.Sprintf("%s-%d-%d", backend, fenced, next+len(plan.blobs))
		plan.blobs = append(plan.blobs, stateBlob{key, all[offs[i]:offs[j]]})
		plan.events = append(plan.events, eventlog.BackendState{Backend: backend, Head: head.Bytes(), Chunk: key, Restart: restart && i == n,
			Entries: j, Sum: hex.EncodeToString(h.Sum(nil))})
		i = j
	}
	if len(plan.events) > 0 {
		return plan, nil
	}
	if keep && bytes.Equal(chain.Head, head.Bytes()) {
		return statePlan{}, nil
	}
	rec := eventlog.BackendState{Backend: backend, Head: head.Bytes(), Restart: restart}
	if keep {
		rec.Entries, rec.Sum = chain.Entries, chain.Sum
	}
	plan.events = []eventlog.Event{rec}
	return plan, nil
}

// encodeEntries joins the entries, each compact and followed by a newline.
// offs holds the start of each entry in the result and then its length.
func encodeEntries(entries []json.RawMessage) ([]byte, []int, error) {
	var buf bytes.Buffer
	offs := make([]int, 1, len(entries)+1)
	for i, e := range entries {
		if err := json.Compact(&buf, e); err != nil {
			return nil, nil, fmt.Errorf("session: backend state entry %d: %w", i, err)
		}
		buf.WriteByte('\n')
		offs = append(offs, buf.Len())
	}
	return buf.Bytes(), offs, nil
}

// splitEntries returns the entries of a blob that holds one entry on each line.
func splitEntries(blob []byte) []json.RawMessage {
	var out []json.RawMessage
	for len(blob) > 0 {
		line, rest, _ := bytes.Cut(blob, []byte{'\n'})
		if len(line) > 0 {
			out = append(out, json.RawMessage(line))
		}
		blob = rest
	}
	return out
}

// snapshotOf joins the blobs of a chain: the head, then each chunk in order.
// A chain of the older form is one blob that starts with the head line.
func snapshotOf(c eventlog.BackendChain, read func(key string) ([]byte, error)) (turn.Snapshot, error) {
	if c.Legacy != "" {
		blob, err := read(c.Legacy)
		if err != nil {
			return turn.Snapshot{}, err
		}
		head, rest, _ := bytes.Cut(blob, []byte{'\n'})
		return turn.Snapshot{Head: json.RawMessage(head), Entries: splitEntries(rest)}, nil
	}
	snap := turn.Snapshot{Head: c.Head}
	for _, key := range c.Chunks {
		blob, err := read(key)
		if err != nil {
			return turn.Snapshot{}, err
		}
		snap.Entries = append(snap.Entries, splitEntries(blob)...)
	}
	if len(snap.Entries) != c.Entries {
		return turn.Snapshot{}, fmt.Errorf("session: backend state chunks hold %d entries, the log says %d", len(snap.Entries), c.Entries)
	}
	return snap, nil
}
