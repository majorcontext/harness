package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/protocol"
)

func (d *runtimeDriver) MessagesPage(t *testing.T, id string, beforeSeq, limit int) callResult {
	t.Helper()
	return d.callPage(t, withQuery("/sessions/"+id+"/messages", "before", beforeSeq, "limit", limit))
}

// MessagesQuery reads a message page with a query string as given.
func (d *runtimeDriver) MessagesQuery(t *testing.T, id, rawQuery string) callResult {
	t.Helper()
	return d.callPage(t, "/sessions/"+id+"/messages?"+rawQuery)
}

// Bootstrap reads the newest messages. The runtime has one page route and
// one event cursor, so the engine bootstrap envelope has no counterpart.
func (d *runtimeDriver) Bootstrap(t *testing.T, id string, limit int) callResult {
	t.Helper()
	return d.callPage(t, withQuery("/sessions/"+id+"/messages", "limit", limit))
}

// callPage reads a message page and reports its messages in the vocabulary of
// the oracle.
func (d *runtimeDriver) callPage(t *testing.T, path string) callResult {
	t.Helper()
	res := d.call(t, http.MethodGet, path, nil)
	if obj, ok := res.Body.(map[string]any); ok {
		if list, ok := obj["messages"].([]any); ok {
			raw, _ := json.Marshal(list)
			var msgs []protocol.Message
			if err := json.Unmarshal(raw, &msgs); err != nil {
				t.Fatalf("GET %s: decode messages: %v (%s)", path, err, raw)
			}
			res.Messages = transcriptOfPage(t, msgs)
			delete(obj, "messages")
		}
	}
	return res
}

// transcriptOfPage maps messages of the runtime to the vocabulary of the oracle.
func transcriptOfPage(t *testing.T, msgs []protocol.Message) []transcriptMessage {
	t.Helper()
	out := make([]transcriptMessage, 0, len(msgs))
	for _, m := range msgs {
		tm := transcriptMessage{ID: m.ID, Role: m.Role, Parts: make([]transcriptPart, 0, len(m.Parts))}
		for _, p := range m.Parts {
			tp := transcriptPart{Type: p.Type, Text: p.Text, CallID: p.CallID, Name: p.Name, IsError: p.IsError, Content: p.Content}
			if len(p.Arguments) > 0 {
				if err := json.Unmarshal(p.Arguments, &tp.Arguments); err != nil {
					t.Fatalf("decode arguments of %s: %v", m.ID, err)
				}
			}
			tm.Parts = append(tm.Parts, tp)
		}
		out = append(out, tm)
	}
	return out
}

// Messages reads the whole conversation, page by page.
func (d *runtimeDriver) Messages(t *testing.T, id string) []transcriptMessage {
	t.Helper()
	var out []transcriptMessage
	for before := 0; ; {
		var page protocol.MessagePage
		d.expect(t, http.StatusOK, http.MethodGet, withQuery("/sessions/"+id+"/messages", "before", before, "limit", protocol.MaxMessageLimit), nil, &page)
		out = append(transcriptOfPage(t, page.Messages), out...)
		if !page.HasMore {
			return out
		}
		before = int(page.FirstSeq)
	}
}

// openAll opens each session of the store that has work to resume, as serve
// does when it starts: a route that only reads a session never opens it, so a
// turn or a queued input that a restart left resumes here.
func (d *runtimeDriver) openAll(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), waitBound)
	defer cancel()
	st := harness.NewDiskStore(d.store)
	for after := ""; ; {
		ids, err := st.Sessions(ctx, after, 100)
		if err != nil {
			t.Fatalf("list sessions: %v", err)
		}
		for _, id := range ids {
			v, err := harness.OpenView(ctx, st, id)
			if err != nil {
				t.Fatalf("view %s: %v", id, err)
			}
			if !v.Resumable() {
				continue
			}
			if _, err := d.rt.Open(ctx, id); err != nil {
				t.Fatalf("open %s: %v", id, err)
			}
		}
		if len(ids) < 100 {
			return
		}
		after = ids[len(ids)-1]
	}
}
