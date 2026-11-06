package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/httpapi"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

var promptStreamAdmissionRows = map[string]runtimeRow{
	"prompt_stream_cancellation_at_admission_cancels_the_committed_input": runtimeOnly("embedded-subscriber-held"),
}

type heldStreamAdmission struct {
	harness.Store
	committed, resume chan struct{}
	seq               uint64
	released          sync.Once
}

func (s *heldStreamAdmission) Append(ctx context.Context, id string, after uint64, records ...[]byte) error {
	if err := s.Store.Append(ctx, id, after, records...); err != nil {
		return err
	}
	for i, record := range records {
		var e struct {
			Kind string `json:"k"`
			Data struct {
				InputID string `json:"input_id"`
			} `json:"d"`
		}
		if err := json.Unmarshal(record, &e); err != nil {
			return err
		}
		if e.Kind == "input.admitted" && e.Data.InputID == "boundary" {
			s.seq = after + uint64(i) + 1
			close(s.committed)
			<-s.resume
		}
	}
	return nil
}

func (s *heldStreamAdmission) release() { s.released.Do(func() { close(s.resume) }) }

func TestContractPromptStreamAdmission(t *testing.T) {
	held := &heldStreamAdmission{committed: make(chan struct{}), resume: make(chan struct{})}
	runScenarios(t, []scenario{{
		name: "prompt_stream_cancellation_at_admission_cancels_the_committed_input",
		driver: func(t *testing.T, h host, modelURL string) driver {
			d := h.newDriver(t, modelURL, nil).(*runtimeDriver)
			d.stop(t, context.Background())
			held.Store = store.NewDiskStore(d.store)
			rt, err := newWired(t, harness.Options{Store: held, Config: d.cfg, WorkDir: d.workDir, Version: runtimeVersion, Getenv: d.getenv})
			if err != nil {
				t.Fatal(err)
			}
			d.rt, d.srv = rt, httptest.NewServer(httpapi.Handler(rt))
			return d
		},
		model:   []harnesstest.Step{{Name: "prior", Repeat: true, Reply: harnesstest.Reply{Text: "prior", Block: true}}},
		actions: []action{create{as: "a"}, watchItems{as: "a"}, submit{as: "a", text: "prior"}, watchItems{as: "a", late: true}, promptStreamAdmissionCanceled{held: held}},
	}})
}

type promptStreamAdmissionCanceled struct{ held *heldStreamAdmission }

func (a promptStreamAdmissionCanceled) run(t *testing.T, r *run) {
	t.Helper()
	s, err := r.drv.(*runtimeDriver).rt.Open(t.Context(), r.id(t, "a"))
	if err != nil {
		t.Fatal(err)
	}
	wait, stop := context.WithTimeout(t.Context(), waitBound)
	defer stop()
	defer a.held.release()
	defer r.fake.Release("prior")
	ctx, cancel := context.WithCancel(wait)
	defer cancel()
	returned := make(chan error, 1)
	go func() {
		stream, err := s.PromptStream(ctx, protocol.Input{ID: "boundary", Parts: []protocol.Part{{Type: protocol.PartText, Text: "go"}}})
		if stream != nil {
			stream.Close()
		}
		returned <- err
	}()
	select {
	case <-a.held.committed:
	case err := <-returned:
		t.Fatalf("prompt returned before the admission boundary: %v", err)
	case <-wait.Done():
		t.Fatal("input did not reach the committed admission boundary")
	}
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("admission boundary error = %v", err)
		}
	case <-wait.Done():
		t.Fatal("canceled admission did not return")
	}
	a.held.release()
	result, err := s.Turn(protocol.Admitted{InputID: "boundary", Seq: a.held.seq}).Wait(wait)
	if err != nil || !result.Withdrawn {
		t.Fatalf("committed input outlived its prompt context: %+v, %v", result, err)
	}
	if s.Snapshot().Status != protocol.StatusRunning {
		t.Fatal("admission cancellation interrupted the prior turn")
	}
	r.fake.Release("prior")
	r.drv.WaitIdle(t, s.ID())
}
