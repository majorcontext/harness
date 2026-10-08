package e2e

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

type recordTimes struct {
	turnInputs map[string]time.Time
	items      map[string]time.Time
	promoted   map[string]time.Time
	compaction time.Time
	resolved   map[string]time.Time
}

func timesOfRecords(t *testing.T, d *runtimeDriver, id string) recordTimes {
	t.Helper()
	out := recordTimes{turnInputs: map[string]time.Time{}, items: map[string]time.Time{}, promoted: map[string]time.Time{}, resolved: map[string]time.Time{}}
	for _, ev := range d.events(t, id) {
		var data struct {
			ItemID    string   `json:"item_id"`
			InputID   string   `json:"input_id"`
			InputIDs  []string `json:"input_ids"`
			RequestID string   `json:"request_id"`
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("decode %s: %v", ev.Kind, err)
		}
		switch ev.Kind {
		case "turn.started":
			for _, in := range data.InputIDs {
				out.turnInputs[in] = ev.Time
			}
		case "item.completed":
			out.items[data.ItemID] = ev.Time
		case "input.promoted":
			out.promoted[data.InputID] = ev.Time
		case "request.resolved":
			out.resolved[data.RequestID] = ev.Time
		case "compaction.applied":
			out.compaction = ev.Time
		}
	}
	return out
}

func TestContractMessageTime(t *testing.T) {
	skipShort(t)
	onHosts(t, func(t *testing.T, h host) {
		t.Run("message_created_at_is_the_envelope_time_of_the_record_that_completed_it", func(t *testing.T) {
			t.Parallel()
			d, _ := startOn(t, h, runtimeWorkdir(t, nil), nil,
				harnesstest.Step{Name: "call", Match: harnesstest.LastUserText("start"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_1", Name: "bash", Input: map[string]any{"command": "echo tool"}},
				}}},
				replyText("done"))
			id := d.Create(t)
			d.expect(t, http.StatusCreated, http.MethodPost, "/sessions/"+id+"/inputs", provInput("in1", "start", "api", "", ""), nil)
			d.WaitIdle(t, id)
			times := timesOfRecords(t, d, id)
			msgs := provPage(t, d, id)
			if len(msgs) != 4 {
				t.Fatalf("messages = %+v, want a prompt, a call, a result, and a reply", msgs)
			}
			for _, m := range msgs {
				want := times.items[m.ID[len("msg_"):]]
				if m.Role == "user" {
					want = times.turnInputs["in1"]
				}
				if want.IsZero() || !m.CreatedAt.Equal(want) {
					t.Errorf("message %s created_at = %v, want the record time %v", m.ID, m.CreatedAt, want)
				}
			}
		})

		t.Run("steer_message_created_at_is_the_time_of_the_last_input_it_joined", func(t *testing.T) {
			t.Parallel()
			d, fake := startOn(t, h, runtimeWorkdir(t, nil), nil,
				harnesstest.Step{Name: "call", Match: harnesstest.LastUserText("start"), Reply: harnesstest.Reply{Block: true, ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_1", Name: "bash", Input: map[string]any{"command": "echo tool"}},
				}}},
				replyText("done"))
			id := d.Create(t)
			d.Submit(t, id, "start")
			if !fake.AwaitRequests(1, waitBound) {
				t.Fatal("the turn sent no model request")
			}
			d.expect(t, http.StatusCreated, http.MethodPost, "/sessions/"+id+"/inputs", provInput("in_a", "alpha", "slack", "slack:C1:1", "Ann"), nil)
			d.expect(t, http.StatusCreated, http.MethodPost, "/sessions/"+id+"/inputs", provInput("in_b", "beta", "api", "cron-7", "Nightly"), nil)
			fake.Release("call")
			d.WaitIdle(t, id)
			times := timesOfRecords(t, d, id)
			m := userMessageWithText(t, provPage(t, d, id), "OPERATOR MESSAGES")
			if want := times.promoted["in_b"]; want.IsZero() || !m.CreatedAt.Equal(want) {
				t.Errorf("steer message created_at = %v, want the time %v of the last promoted input", m.CreatedAt, want)
			}
		})

		t.Run("compaction_summary_created_at_is_the_time_of_the_compaction_record", func(t *testing.T) {
			t.Parallel()
			d, _ := startOn(t, h, runtimeWorkdir(t, nil), map[string]any{"compaction_keep_turns": 1},
				harnesstest.Step{Name: "one", Match: harnesstest.LastUserText("one"), Reply: harnesstest.Reply{Text: "1"}},
				harnesstest.Step{Name: "two", Match: harnesstest.LastUserText("two"), Reply: harnesstest.Reply{Text: "2"}},
				harnesstest.Step{Name: "summary", Reply: harnesstest.Reply{Text: "gist"}})
			id := d.Create(t)
			d.Submit(t, id, "one")
			d.WaitIdle(t, id)
			d.Submit(t, id, "two")
			d.WaitIdle(t, id)
			if res := d.Compact(t, id); res.Status != http.StatusOK {
				t.Fatalf("compact = %d %v", res.Status, res.Body)
			}
			times := timesOfRecords(t, d, id)
			var page protocol.MessagePage
			d.expect(t, http.StatusOK, http.MethodGet, "/sessions/"+id+"/messages?limit=1000", nil, &page)
			if len(page.Messages) == 0 {
				t.Fatal("the page holds no message")
			}
			if got := page.Messages[0].CreatedAt; times.compaction.IsZero() || !got.Equal(times.compaction) {
				t.Errorf("summary created_at = %v, want the compaction time %v", got, times.compaction)
			}
		})

		t.Run("dismissal_message_created_at_is_the_time_of_the_request_resolved_record", func(t *testing.T) { dismissalTime(t, h) })
	})
}

func dismissalTime(t *testing.T, h host) {
	t.Helper()
	fake := harnesstest.New(t)
	drv := claudeLane{mode: "question", ask: true}.newDriver(t, h, fake.URL()).(*claudeDriver)
	d := drv.laneHost.(*runtimeDriver)
	id := d.Create(t)
	d.Submit(t, id, "pick a db")
	d.WaitIdle(t, id)
	if res := drv.resolveQuestion(t, id, "toolu_q", resolution{dismiss: true}); res.Status != http.StatusNoContent {
		t.Fatalf("dismiss = %d %v", res.Status, res.Body)
	}
	times := timesOfRecords(t, d, id)
	want := times.resolved["toolu_q"]
	var found bool
	for _, m := range provPage(t, d, id) {
		if m.ID != "msg_resolved_toolu_q" {
			continue
		}
		found = true
		if want.IsZero() || !m.CreatedAt.Equal(want) {
			t.Errorf("dismissal created_at = %v, want the request.resolved time %v", m.CreatedAt, want)
		}
	}
	if !found {
		t.Fatal("the page holds no dismissal message")
	}
}
