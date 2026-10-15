package e2e

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
)

// expectBannerStable checks that request n+1, which a new process sends, keeps
// the system prompt, the tools, and every message of request n byte for byte,
// and that request n carries the engine banner in the message at index at.
type expectBannerStable struct{ n, at int }

func (a expectBannerStable) run(t *testing.T, r *run) {
	t.Helper()
	reqs := r.fake.Requests()
	if len(reqs) <= a.n {
		t.Fatalf("model saw %d requests, want more than %d", len(reqs), a.n)
	}
	if reqs[a.n-1].System != reqs[a.n].System {
		t.Fatalf("system prompt of request %d differs from request %d\n got: %q\nwant: %q", a.n+1, a.n, reqs[a.n].System, reqs[a.n-1].System)
	}
	if got, want := mustJSON(t, reqs[a.n].Tools), mustJSON(t, reqs[a.n-1].Tools); !bytes.Equal(got, want) {
		t.Fatalf("tools of request %d differ from request %d\n got: %s\nwant: %s", a.n+1, a.n, got, want)
	}
	before, after := reqs[a.n-1].Messages, reqs[a.n].Messages
	if a.at >= len(before) || !bytes.Contains(mustJSON(t, before[a.at]), []byte("[engine: harness ")) {
		t.Fatalf("message %d of request %d holds no engine banner: %s", a.at, a.n, mustJSON(t, before))
	}
	if !bytes.Contains(mustJSON(t, before), []byte("[engine: harness ")) {
		t.Fatalf("request %d holds no engine banner: %s", a.n, mustJSON(t, before))
	}
	if len(after) < len(before) {
		t.Fatalf("request %d has %d messages, fewer than the %d of request %d", a.n+1, len(after), len(before), a.n)
	}
	if got, want := mustJSON(t, after[:len(before)]), mustJSON(t, before); !bytes.Equal(got, want) {
		t.Fatalf("history prefix of request %d differs from request %d\n got: %s\nwant: %s", a.n+1, a.n, got, want)
	}
}

// expectBannerCreated checks that the banner of every request names the
// created time of the session, never a process start time.
type expectBannerCreated struct{ as string }

func (a expectBannerCreated) run(t *testing.T, r *run) {
	t.Helper()
	res := r.drv.GetSession(t, r.id(t, a.as))
	body, _ := res.Body.(map[string]any)
	raw, _ := body["created_at"].(string)
	created, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("created_at %q: %v", raw, err)
	}
	want := " · session started " + created.UTC().Format(time.RFC3339) + "]"
	reqs := r.fake.Requests()
	if len(reqs) == 0 {
		t.Fatal("model saw no request")
	}
	for i, req := range reqs {
		text := string(mustJSON(t, req.Messages))
		if !strings.Contains(text, want) || strings.Contains(text, "engine started") {
			t.Fatalf("request %d banner does not end with %q: %s", i+1, want, text)
		}
	}
}

func TestContractEngineBanner(t *testing.T) {
	text := func(name, user, reply string) harnesstest.Step {
		return harnesstest.Step{Name: name, Match: harnesstest.LastUserText(user), Reply: harnesstest.Reply{Text: reply}}
	}
	runScenarios(t, []scenario{
		{
			name:  "banner_and_history_prefix_stay_the_same_after_a_restart",
			model: []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2"), text("three", "three", "3")},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				restart{},
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				expectBannerStable{n: 2, at: 0},
			},
		},
		{
			name:   "banner_and_history_prefix_stay_the_same_after_a_compaction_and_a_restart",
			config: map[string]any{"compaction_keep_turns": 1},
			model: []harnesstest.Step{
				text("one", "one", "1"), text("two", "two", "2"),
				{Name: "summary", Match: harnesstest.SystemContains("You are summarizing a prefix"), Reply: harnesstest.Reply{Text: "gist"}},
				text("three", "three", "3"), text("four", "four", "4"),
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				compact{as: "a"},
				submit{as: "a", text: "three"}, waitIdle{as: "a"},
				restart{},
				submit{as: "a", text: "four"}, waitIdle{as: "a"},
				expectBannerStable{n: 4, at: 2},
			},
		},
		{
			name:  "banner_names_the_created_time_of_the_session",
			model: []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2")},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				restart{},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				expectBannerCreated{as: "a"},
			},
		},
	})
}
