package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

func newStoreServer(t *testing.T, store engine.SessionStore, prov provider.Provider, mutate ...func(*Options)) *harness {
	t.Helper()
	const token = "secret-run-token"
	model := message.ModelRef{Provider: prov.Name(), Model: "m1"}
	var srv *Server
	mkCfg := func(m message.ModelRef) engine.Config {
		if m.IsZero() {
			m = model
		}
		return engine.Config{
			Providers:    provider.Registry{prov.Name(): prov},
			Model:        m,
			SessionStore: store,
			OnEvent:      func(ev engine.Event) { srv.Publish(ev) },
		}
	}
	opts := Options{
		Store:             store,
		RunToken:          token,
		HeartbeatInterval: 5 * time.Millisecond,
		NewSession: func(m message.ModelRef, workDir string, parent string) (*engine.Session, error) {
			cfg := mkCfg(m)
			cfg.WorkDir = workDir
			cfg.ParentSession = parent
			return engine.NewSession(cfg), nil
		},
		LoadSession: func(id string) (*engine.Session, error) {
			return engine.LoadSession(mkCfg(message.ModelRef{}), id)
		},
	}
	for _, m := range mutate {
		m(&opts)
	}
	srv, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &harness{t: t, token: token, srv: srv, ts: ts}
}

type sseFrame struct {
	id string
	ev Event
}

// replay reads the SSE replay batch: every frame before the first heartbeat.
func (h *harness) replay(path string, header map[string]string) []sseFrame {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", h.ts.URL+path, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []sseFrame
	var cur sseFrame
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, ": heartbeat"):
			return out
		case strings.HasPrefix(line, "id: "):
			cur.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.ev); err != nil {
				h.t.Fatalf("decode frame: %v", err)
			}
			out = append(out, cur)
			cur = sseFrame{}
		}
	}
	h.t.Fatalf("stream ended before a heartbeat: %v", sc.Err())
	return nil
}

func messageTexts(frames []sseFrame) []string {
	var out []string
	for _, f := range frames {
		if f.ev.Type == evtMessage && f.ev.Message != nil && f.ev.Message.Role == message.RoleAssistant {
			out = append(out, msgText(f.ev.Message))
		}
	}
	return out
}

func twoReplies() *scriptedProvider {
	return &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("first reply"), asstTurn("second reply")}}
}

func TestMessagePagesOnMemStore(t *testing.T) {
	h := newStoreServer(t, engine.NewMemStore(), twoReplies())
	id := createSessionDirect(t, h.srv, "test/m1")
	h.promptAndWaitIdle(id, "one")
	h.promptAndWaitIdle(id, "two")

	page := getPage(t, h, id, "?limit=1")
	if len(page.Messages) != 1 || msgText(&page.Messages[0]) != "second reply" {
		t.Fatalf("newest page = %+v, want the second reply", page.Messages)
	}
	older := getPage(t, h, id, "?limit=1&before_seq="+strconv.Itoa(page.FirstSeq))
	if len(older.Messages) != 1 || msgText(&older.Messages[0]) != "two" {
		t.Fatalf("older page = %+v, want the second prompt", older.Messages)
	}
	older = getPage(t, h, id, "?limit=1&before_seq="+strconv.Itoa(older.FirstSeq))
	if len(older.Messages) != 1 || msgText(&older.Messages[0]) != "first reply" {
		t.Fatalf("page before = %+v, want the first reply", older.Messages)
	}
}

func TestSSEReplayOnMemStore(t *testing.T) {
	h := newStoreServer(t, engine.NewMemStore(), twoReplies())
	id := createSessionDirect(t, h.srv, "test/m1")
	h.promptAndWaitIdle(id, "one")
	h.promptAndWaitIdle(id, "two")

	frames := h.replay("/event?session="+id+"&from=0", nil)
	got := messageTexts(frames)
	if len(got) != 2 || got[0] != "first reply" || got[1] != "second reply" {
		t.Fatalf("replayed replies = %q", got)
	}
	var last int64
	for _, f := range frames {
		if f.id != strconv.FormatInt(f.ev.Seq, 10) {
			t.Errorf("frame id %q != seq %d", f.id, f.ev.Seq)
		}
		if f.ev.Seq <= last {
			t.Errorf("seq %d after %d", f.ev.Seq, last)
		}
		last = f.ev.Seq
	}
}

func TestSecondServerReplaysFromSeq(t *testing.T) {
	store := engine.NewMemStore()
	prov := twoReplies()
	a := newStoreServer(t, store, prov)
	id := createSessionDirect(t, a.srv, "test/m1")
	a.promptAndWaitIdle(id, "one")
	frames := a.replay("/event?from=0", nil)
	n := frames[len(frames)-1].ev.Seq
	if err := a.srv.Close(); err != nil {
		t.Fatal(err)
	}

	b := newStoreServer(t, store, prov)
	from := strconv.FormatInt(n-1, 10)
	for name, got := range map[string][]sseFrame{
		"from":          b.replay("/event?from="+from, nil),
		"Last-Event-ID": b.replay("/event", map[string]string{"Last-Event-ID": from}),
	} {
		if len(got) != 1 || got[0].ev.Seq != n {
			t.Fatalf("%s replay = %+v, want only the event with seq %d", name, got, n)
		}
	}

	b.promptAndWaitIdle(id, "two")
	next := b.replay("/event?from="+strconv.FormatInt(n, 10), nil)
	if len(next) == 0 || next[0].ev.Seq != n+1 {
		t.Fatalf("first event after the restart = %+v, want seq %d", next, n+1)
	}
}

func TestEventLogIDMustNotBeSessionID(t *testing.T) {
	id := engine.NewSession(engine.Config{SessionStore: engine.NewMemStore()}).ID
	opts := testOptions(t)
	opts.RunToken = "tok"
	opts.EventLogID = id
	opts.NewSession = func(message.ModelRef, string, string) (*engine.Session, error) { return nil, errors.New("unused") }
	opts.LoadSession = func(string) (*engine.Session, error) { return nil, errors.New("unused") }
	if _, err := New(opts); err == nil {
		t.Fatalf("New accepted EventLogID %q", id)
	}
}

func TestNoWorktreeSweepOnMemStore(t *testing.T) {
	dir := t.TempDir()
	h := newStoreServer(t, engine.NewMemStore(), twoReplies(), func(o *Options) { o.SessionDir = dir })
	if h.srv.worktreeBase != "" {
		t.Fatalf("worktreeBase = %q on a memory store", h.srv.worktreeBase)
	}
	id := createSessionDirect(t, h.srv, "test/m1")
	h.promptAndWaitIdle(id, "one")
	for _, name := range []string{"worktrees", journalName} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s exists in SessionDir on a memory store: %v", name, err)
		}
	}
}

func TestEventLogFencedByAnotherWriter(t *testing.T) {
	store := engine.NewMemStore()
	coll := newErrCollector()
	h := newStoreServer(t, store, twoReplies(), func(o *Options) { o.OnError = coll.onError })
	createSessionDirect(t, h.srv, "test/m1")
	before, err := store.Len(defaultEventLogID)
	if err != nil {
		t.Fatal(err)
	}
	intruder := []byte(`{"type":"intruder","seq":9999}`)
	if err := store.Append(defaultEventLogID, before, intruder); err != nil {
		t.Fatal(err)
	}

	createSessionDirect(t, h.srv, "test/m1")
	<-coll.ch
	if err := coll.last(); !errors.Is(err, engine.ErrAppendConflict) {
		t.Fatalf("OnError = %v, want ErrAppendConflict", err)
	}
	recs, err := store.Load(defaultEventLogID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != before+1 || string(recs[before]) != string(intruder) {
		t.Fatalf("events log has %d records, want %d ending in the intruder record", len(recs), before+1)
	}
}

func msgText(m *message.Message) string {
	var b strings.Builder
	for _, p := range m.Parts {
		if t, ok := p.(*message.Text); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}
