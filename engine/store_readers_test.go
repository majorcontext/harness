package engine_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

type countingProvider struct{ n int }

func (p *countingProvider) Name() string { return "mem" }

func (p *countingProvider) Stream(context.Context, *provider.Request) (provider.Stream, error) {
	p.n++
	msg := &message.Message{
		ID:    fmt.Sprintf("msg_a%d", p.n),
		Role:  message.RoleAssistant,
		Parts: message.Parts{&message.Text{Text: fmt.Sprintf("answer %d", p.n)}},
	}
	return &replyStream{ev: provider.Event{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn}}, nil
}

func readerStores(t *testing.T) map[string]engine.SessionStore {
	t.Helper()
	return map[string]engine.SessionStore{
		"disk": engine.NewDiskStore(t.TempDir(), engine.DiskStoreOptions{}),
		"mem":  engine.NewMemStore(),
	}
}

func buildReaderSession(t *testing.T, st engine.SessionStore) string {
	t.Helper()
	cfg := engine.Config{
		SessionStore:         st,
		Providers:            provider.Registry{"mem": &countingProvider{}},
		Model:                message.ModelRef{Provider: "mem", Model: "m"},
		SnapshotEveryRecords: 1,
	}
	s := engine.NewSession(cfg)
	for i := 1; i <= 5; i++ {
		if _, err := s.Prompt(context.Background(), fmt.Sprintf("question %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordCommand(message.CommandRecord{
		ID: engine.NewCommandID(), Line: "/compact", Name: "compact",
		Source: message.PromptSourceTyped, Status: message.CommandSucceeded,
		AfterMessageID: "msg_a5",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PersistErr(); err != nil {
		t.Fatal(err)
	}
	s.WaitSnapshots()
	return s.ID
}

func messageTexts(p engine.MessagePage) []string {
	var out []string
	for _, m := range p.Messages {
		out = append(out, m.Parts.Text())
	}
	return out
}

func TestReadersAgreeAcrossStores(t *testing.T) {
	type shape struct {
		Texts              [][]string
		Total, First, Last [2]int
		More               [2]bool
		Commands           []string
	}
	shapes := map[string]shape{}
	for name, st := range readerStores(t) {
		id := buildReaderSession(t, st)
		first, err := engine.ReadMessagePageFrom(st, id, 0, 3)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if want := []string{"question 5", "answer 5"}; !slices.Equal(messageTexts(first)[1:], want) {
			t.Errorf("%s: newest page texts = %q, want tail %q", name, messageTexts(first), want)
		}
		if first.Total != 10 || first.FirstSeq != 8 || first.LastSeq != 10 || !first.HasMore {
			t.Errorf("%s: first page = total %d seq %d..%d more %v, want 10, 8..10, true", name, first.Total, first.FirstSeq, first.LastSeq, first.HasMore)
		}
		second, err := engine.ReadMessagePageFrom(st, id, first.FirstSeq, 3)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if second.FirstSeq != 5 || second.LastSeq != 7 {
			t.Errorf("%s: second page seq %d..%d, want 5..7", name, second.FirstSeq, second.LastSeq)
		}
		if len(first.Commands) != 1 || first.Commands[0].Name != "compact" {
			t.Errorf("%s: first page commands = %+v, want the compact command", name, first.Commands)
		}
		shapes[name] = shape{
			Texts:    [][]string{messageTexts(first), messageTexts(second)},
			Total:    [2]int{first.Total, second.Total},
			First:    [2]int{first.FirstSeq, second.FirstSeq},
			Last:     [2]int{first.LastSeq, second.LastSeq},
			More:     [2]bool{first.HasMore, second.HasMore},
			Commands: []string{first.Commands[0].Name, first.Commands[0].AfterMessageID},
		}
	}
	if !reflect.DeepEqual(shapes["disk"], shapes["mem"]) {
		t.Errorf("disk pages %+v differ from mem pages %+v", shapes["disk"], shapes["mem"])
	}
}

func TestReadSessionIndexFromMem(t *testing.T) {
	var ixs []engine.SessionIndex
	var infos []engine.SessionInfo
	for name, st := range readerStores(t) {
		id := buildReaderSession(t, st)
		ix, err := engine.ReadSessionIndexFrom(st, id)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		info, err := engine.ReadSessionInfoFrom(st, id)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if ix.ID != id || info.ID != id {
			t.Errorf("%s: ids %q, %q, want %q", name, ix.ID, info.ID, id)
		}
		ixs = append(ixs, ix)
		infos = append(infos, info)
	}
	if ixs[0].Messages != ixs[1].Messages || ixs[0].Usage != ixs[1].Usage || ixs[0].DurableMessages != ixs[1].DurableMessages {
		t.Errorf("indexes differ: %+v vs %+v", ixs[0], ixs[1])
	}
	if infos[0].Messages != infos[1].Messages || infos[0].Usage != infos[1].Usage {
		t.Errorf("infos differ: %+v vs %+v", infos[0], infos[1])
	}
}

func TestLoadJournalFromMem(t *testing.T) {
	types := map[string][]string{}
	for name, st := range readerStores(t) {
		id := buildReaderSession(t, st)
		recs, err := engine.LoadJournalFrom(st, id)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, r := range recs {
			types[name] = append(types[name], r.Type)
		}
		if len(recs) == 0 || recs[0].Type != "session" {
			t.Errorf("%s: first record = %+v, want session header", name, recs)
		}
		if _, err := engine.LoadJournalFrom(st, engine.NewSession(engine.Config{}).ID); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: unknown id error = %v, want fs.ErrNotExist", name, err)
		}
	}
	if !slices.Equal(types["disk"], types["mem"]) {
		t.Errorf("record types differ: disk %q, mem %q", types["disk"], types["mem"])
	}
}

func TestListSessionIDsFromFiltersNonSessionLogs(t *testing.T) {
	for name, st := range readerStores(t) {
		id := buildReaderSession(t, st)
		if err := st.Append("events", 0, []byte(`{"type":"event"}`)); err != nil {
			t.Fatal(err)
		}
		if err := st.Append(id+".claude-code", 0, []byte(`{"type":"mirror"}`)); err != nil {
			t.Fatal(err)
		}
		got, err := engine.ListSessionIDsFrom(st)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !slices.Equal(got, []string{id}) {
			t.Errorf("%s: ids = %q, want only %q", name, got, id)
		}
	}
}

func TestSessionExistsIn(t *testing.T) {
	for name, st := range readerStores(t) {
		id := buildReaderSession(t, st)
		if !engine.SessionExistsIn(st, id) {
			t.Errorf("%s: SessionExistsIn(%q) = false", name, id)
		}
		for _, bad := range []string{engine.NewSession(engine.Config{}).ID, "../x", ""} {
			if engine.SessionExistsIn(st, bad) {
				t.Errorf("%s: SessionExistsIn(%q) = true", name, bad)
			}
		}
	}
}

func TestSessionExistsInDiskMatchesSessionExistsForDamagedJournal(t *testing.T) {
	dir := t.TempDir()
	st := engine.NewDiskStore(dir, engine.DiskStoreOptions{})
	id := engine.NewSession(engine.Config{}).ID
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(`{"type":"sess`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !engine.SessionExists(dir, id) {
		t.Fatal("SessionExists = false for a journal with a torn first record")
	}
	if !engine.SessionExistsIn(st, id) {
		t.Errorf("SessionExistsIn = false, want true to match SessionExists")
	}
}
