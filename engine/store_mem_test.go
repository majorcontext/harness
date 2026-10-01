package engine_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/engine/storetest"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

type replyProvider struct {
	text  string
	tools [][]provider.ToolDef
}

func (p *replyProvider) Name() string { return "mem" }

func (p *replyProvider) Stream(_ context.Context, req *provider.Request) (provider.Stream, error) {
	p.tools = append(p.tools, req.Tools)
	msg := &message.Message{ID: "msg_a", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: p.text}}}
	return &replyStream{ev: provider.Event{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn}}, nil
}

type replyStream struct {
	ev   provider.Event
	done bool
}

func (s *replyStream) Next() (provider.Event, error) {
	if s.done {
		return provider.Event{}, io.EOF
	}
	s.done = true
	return s.ev, nil
}

func (s *replyStream) Close() error { return nil }

func memConfig(st engine.SessionStore, prov *replyProvider) engine.Config {
	return engine.Config{
		SessionStore:          st,
		Providers:             provider.Registry{"mem": prov},
		Model:                 message.ModelRef{Provider: "mem", Model: "m"},
		ToolResultInlineBytes: 64,
		SnapshotEveryRecords:  1,
	}
}

func TestMemStoreConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) engine.SessionStore { return engine.NewMemStore() })
}

func TestMemStoreIsolatesCallerBuffers(t *testing.T) {
	st := engine.NewMemStore()
	in := []byte("a")
	if err := st.Append("ses_a", 0, in); err != nil {
		t.Fatal(err)
	}
	in[0] = 'X'
	got, err := st.Load("ses_a")
	if err != nil {
		t.Fatal(err)
	}
	if string(got[0]) != "a" {
		t.Fatalf("Load after caller mutation = %q, want %q", got[0], "a")
	}
	got[0][0] = 'Y'
	again, _ := st.Load("ses_a")
	if string(again[0]) != "a" {
		t.Fatalf("Load after loaded mutation = %q, want %q", again[0], "a")
	}
	hdr, _ := st.Header("ses_a")
	hdr[0] = 'Z'
	if again, _ = st.Load("ses_a"); string(again[0]) != "a" {
		t.Fatalf("Load after header mutation = %q, want %q", again[0], "a")
	}
	blob := []byte("b")
	if err := st.PutBlob("ses_a", "n", blob); err != nil {
		t.Fatal(err)
	}
	blob[0] = 'X'
	b, _ := st.GetBlob("ses_a", "n")
	if string(b) != "b" {
		t.Fatalf("GetBlob after caller mutation = %q, want %q", b, "b")
	}
	b[0] = 'Y'
	if b2, _ := st.GetBlob("ses_a", "n"); string(b2) != "b" {
		t.Fatalf("GetBlob after returned mutation = %q, want %q", b2, "b")
	}
}

func roundTrip(t *testing.T, mem *engine.MemStore) (engine.Config, string, *replyProvider) {
	t.Helper()
	prov := &replyProvider{text: "hello"}
	cfg := memConfig(mem, prov)
	s := engine.NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if err := s.PersistErr(); err != nil {
		t.Fatalf("PersistErr = %v", err)
	}
	return cfg, s.ID, prov
}

func TestSessionRoundTripOnMemStore(t *testing.T) {
	mem := engine.NewMemStore()
	_, id, prov := roundTrip(t, mem)

	loaded, err := engine.LoadSession(memConfig(mem, prov), id)
	if err != nil {
		t.Fatal(err)
	}
	h := loaded.History()
	if len(h) != 2 {
		t.Fatalf("History = %+v, want user and assistant messages", h)
	}
	if h[0].Role != message.RoleUser || h[0].Parts.Text() != "hi" {
		t.Errorf("History[0] = %+v, want user text hi", h[0])
	}
	if h[1].Role != message.RoleAssistant || h[1].Parts.Text() != "hello" {
		t.Errorf("History[1] = %+v, want assistant text hello", h[1])
	}
}

func TestMemStoreSessionHasNoDiskSidecars(t *testing.T) {
	mem := engine.NewMemStore()
	_, id, prov := roundTrip(t, mem)

	for _, tools := range prov.tools {
		for _, d := range tools {
			if d.Name == "read_tool_result" {
				t.Fatalf("read_tool_result advertised on a MemStore session")
			}
		}
	}
	if _, err := engine.LoadSession(memConfig(mem, prov), id); err != nil {
		t.Fatalf("LoadSession = %v", err)
	}
	for _, name := range []string{"snapshot", "index"} {
		if b, err := mem.GetBlob(id, name); err == nil {
			t.Errorf("blob %q = %q, want absent", name, b)
		}
	}
}

var errBoom = errors.New("boom")

type failingStore struct{ engine.SessionStore }

func (failingStore) Append(string, int, ...[]byte) error { return errBoom }

func TestPersistErrSurfaces(t *testing.T) {
	prov := &replyProvider{text: "hello"}
	s := engine.NewSession(memConfig(failingStore{engine.NewMemStore()}, prov))
	if _, err := s.Prompt(context.Background(), "hi"); err != nil {
		t.Logf("Prompt = %v", err)
	}
	if err := s.PersistErr(); !errors.Is(err, errBoom) {
		t.Fatalf("PersistErr = %v, want errBoom", err)
	}
}
