package e2e

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
)

// expectBannerStable checks that request n+1, which a new process sends, keeps
// every message of request n byte for byte, and that request n carries the
// engine banner.
type expectBannerStable struct{ n int }

func (a expectBannerStable) run(t *testing.T, r *run) {
	t.Helper()
	reqs := r.fake.Requests()
	if len(reqs) <= a.n {
		t.Fatalf("model saw %d requests, want more than %d", len(reqs), a.n)
	}
	before, after := reqs[a.n-1].Messages, reqs[a.n].Messages
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
				expectBannerStable{n: 2},
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
