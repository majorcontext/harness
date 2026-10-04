package session

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"sync"
	"testing"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
)

type memLog struct {
	mu    sync.Mutex
	recs  []eventlog.Record
	blobs map[string][]byte
}

func (l *memLog) PutBlob(_ context.Context, key string, r io.Reader) error {
	b, err := io.ReadAll(r)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.blobs == nil {
		l.blobs = map[string][]byte{}
	}
	l.blobs[key] = b
	return err
}

func (l *memLog) GetBlob(_ context.Context, key string) (io.ReadCloser, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.blobs[key]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (l *memLog) Head(context.Context) (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return uint64(len(l.recs)), nil
}

func (l *memLog) Append(_ context.Context, _ uint64, records ...[]byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range records {
		l.recs = append(l.recs, eventlog.Record{Seq: uint64(len(l.recs)) + 1, Data: r})
	}
	return nil
}

func (l *memLog) Read(_ context.Context, after uint64, limit int) ([]eventlog.Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.recs[after:min(len(l.recs), int(after)+limit)], nil
}

type owned struct{}

func (owned) Epoch() uint64         { return 1 }
func (owned) Lost() <-chan struct{} { return nil }
func (owned) Release()              {}

func TestFramesNeverTrailTheDurableHead(t *testing.T) {
	const appends = 3000
	a := newActor(Config{ID: "s1", Store: &memLog{}, Ownership: owned{}, Backend: newHeldBackend(false), Base: t.Context()}, &eventlog.State{})
	if err := a.appendCtx(t.Context(), eventlog.SessionCreated{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range appends {
			if err := a.appendCtx(t.Context(), eventlog.SettingsChanged{Effort: new([]string{"low", "high"}[i%2])}); err != nil {
				t.Error(err)
			}
		}
	}()
	go func() {
		t := &turnRun{a: a, r: &running{id: "t1"}}
		for {
			select {
			case <-done:
				return
			default:
				t.Delta("i1", turn.Delta{Type: eventlog.PartText, Text: "x"})
			}
		}
	}()
	var head uint64
	for e, err := range a.Events(t.Context(), 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Ephemeral && e.Seq < head {
			t.Errorf("%s frame at seq %d came after durable seq %d", e.Kind, e.Seq, head)
			break
		}
		if !e.Ephemeral {
			head = e.Seq
		}
		if head == appends+1 {
			break
		}
	}
	<-done
}
