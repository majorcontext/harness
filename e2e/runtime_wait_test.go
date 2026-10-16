package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/majorcontext/harness/protocol"
)

// stream reads the SSE events of session id after seq after, and passes
// each to visit until it returns true. The read fails at waitBound.
func (d *runtimeDriver) stream(t *testing.T, id string, after uint64, header bool, visit func(sseID string, ev protocol.Event) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), waitBound)
	defer cancel()
	h := http.Header{"Accept": {"text/event-stream"}}
	path := "/sessions/" + id + "/events"
	if header {
		h.Set("Last-Event-ID", strconv.FormatUint(after, 10))
	} else {
		path += "?after=" + strconv.FormatUint(after, 10)
	}
	resp := d.send(t, ctx, http.MethodGet, path, nil, h)
	defer func() { _ = resp.Body.Close() }()
	sc := newSSEScanner(resp.Body)
	for {
		raw, err := sc.next()
		if err != nil {
			t.Fatalf("event stream of %s ended: %v", id, err)
		}
		var ev protocol.Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("decode frame %s: %v", raw, err)
		}
		if visit(sc.id, ev) {
			return
		}
	}
}

// settled reports whether a session runs nothing and has nothing to run. A
// session that waits for an answer runs nothing, and neither does one whose
// queue waits after a turn that ended provider_exhausted.
func settled(v protocol.Session) bool {
	held := v.LastTurn != nil && v.LastTurn.Cause == "provider_exhausted"
	return (v.Status == protocol.StatusIdle || v.Status == protocol.StatusWaiting) && (len(v.Queued) == 0 || held) && (v.Goal == nil || v.Goal.State != "active")
}

// WaitIdle reads the view again on each frame of the session, from the head
// that the first view saw, until the session has settled. A child has
// settled when its parent has recorded that and has settled too, as the
// report of the child can start a turn of the parent.
func (d *runtimeDriver) WaitIdle(t *testing.T, id string) {
	t.Helper()
	v := d.view(t, id)
	if !settled(v) {
		d.stream(t, id, v.HeadSeq, false, func(string, protocol.Event) bool {
			v = d.view(t, id)
			return settled(v)
		})
	}
	if v.ParentID != "" {
		d.AwaitChildSettled(t, v.ParentID, id)
		d.WaitIdle(t, v.ParentID)
	}
}

// AwaitChildSettled waits until the newest child.spawned of child in the
// log of parent has a child.settled after it.
func (d *runtimeDriver) AwaitChildSettled(t *testing.T, parent, child string) {
	t.Helper()
	settledLast := func(ev protocol.Event) (mine, done bool) {
		if ev.Kind != "child.spawned" && ev.Kind != "child.settled" {
			return false, false
		}
		c := decodeEvent[struct {
			ChildID string `json:"child_id"`
		}](t, ev)
		return c.ChildID == child, ev.Kind == "child.settled"
	}
	events := d.events(t, parent)
	for i := len(events) - 1; i >= 0; i-- {
		if mine, done := settledLast(events[i]); mine {
			if done {
				return
			}
			break
		}
	}
	var head uint64
	if len(events) > 0 {
		head = events[len(events)-1].Seq
	}
	d.stream(t, parent, head, false, func(_ string, ev protocol.Event) bool {
		mine, done := settledLast(ev)
		return mine && done
	})
}

func (d *runtimeDriver) AwaitGoalExhausted(t *testing.T) {
	t.Helper()
	for _, id := range d.sessionIDs(t) {
		if g := d.view(t, id).Goal; g == nil {
			continue
		}
		d.stream(t, id, 0, false, func(_ string, ev protocol.Event) bool {
			return ev.Kind == "goal.changed" && decodeEvent[struct{ State string }](t, ev).State == "exhausted"
		})
		return
	}
	t.Fatal("no session has a goal")
}

// Child waits on the events of the parent until it has spawned more than
// nth children, and returns the nth in spawn order.
func (d *runtimeDriver) Child(t *testing.T, parentID string, nth int) string {
	t.Helper()
	var ids []string
	d.stream(t, parentID, 0, false, func(_ string, ev protocol.Event) bool {
		if ev.Kind == "child.spawned" && !ev.Ephemeral {
			ids = append(ids, decodeEvent[struct {
				ChildID string `json:"child_id"`
			}](t, ev).ChildID)
		}
		return len(ids) > nth
	})
	return ids[nth]
}

// SSEResume reads the durable frames of one session after afterSeq, up to
// its head on entry. A read of every session is the deleted box-global stream.
func (d *runtimeDriver) SSEResume(t *testing.T, id string, afterSeq int64, header, scoped bool) callResult {
	t.Helper()
	if id == "" {
		return deleted("GET /event")
	}
	head := d.view(t, id).HeadSeq
	body := []any{}
	if uint64(afterSeq) < head {
		d.stream(t, id, uint64(afterSeq), header, func(sseID string, ev protocol.Event) bool {
			if ev.Ephemeral {
				return false
			}
			body = append(body, map[string]any{"id": sseID, "type": ev.Kind, "seq": ev.Seq})
			return ev.Seq >= head
		})
	}
	return callResult{Status: http.StatusOK, Body: map[string]any{"frames": body}}
}

func partsText(parts []logPart) string {
	var texts []string
	for _, p := range parts {
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, "\n")
}
