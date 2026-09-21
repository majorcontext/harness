package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/majorcontext/harness/message"
)

// TestAvailableDuringTaskMatchesTheRoute is the behavioral check behind the
// available_during_task field: a client that trusts the flag must not be
// refused for a run-slot conflict, and one the flag warns about must be.
//
// Asserting the registry against itself would pass no matter what the
// routes do. So this drives every control command's own method and path,
// taken from GET /commands, against a session whose turn is genuinely in
// flight, and compares the refusal to the flag.
//
// Each command gets its own harness and its own parked turn: /abort ends
// the very turn the others need held open.
func TestAvailableDuringTaskMatchesTheRoute(t *testing.T) {
	type entry struct {
		Name                string `json:"name"`
		Kind                string `json:"kind"`
		Method              string `json:"method"`
		Path                string `json:"path"`
		AvailableDuringTask *bool  `json:"available_during_task"`
	}

	// A minimal valid body per command, so a refusal can only be the run
	// slot and never a malformed request.
	bodies := map[string]any{
		"model":    map[string]string{"model": "root/m2"},
		"thinking": map[string]string{"effort": "low"},
		"tier":     map[string]string{"service_tier": "standard"},
		"goal":     map[string]string{"condition": "the tests pass"},
	}

	h := newHarness(t, newBlockingProvider("root"))
	_, data := h.do("GET", "/commands", nil)
	var body struct {
		Commands []entry `json:"commands"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("GET /commands: %v", err)
	}

	var control int
	for _, c := range body.Commands {
		if c.Kind != "control" {
			if c.AvailableDuringTask != nil {
				t.Errorf("%s command %q carries available_during_task; it has no route", c.Kind, c.Name)
			}
			continue
		}
		control++
		if c.AvailableDuringTask == nil {
			t.Errorf("control command %q omits available_during_task", c.Name)
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			blocker := newBlockingProvider("root")
			t.Cleanup(blocker.releaseAll)
			// A goal evaluator must be configured, or /goal is rejected
			// with 400 before it ever reaches the run-slot claim this
			// test exists to observe.
			dir := t.TempDir()
			srv := newServer(t, dir, blocker, 0, func(o *Options) {
				o.GoalEvaluator = message.ModelRef{Provider: blocker.Name(), Model: "eval"}
			})
			ts := httptest.NewServer(srv)
			t.Cleanup(ts.Close)
			th := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv, ts: ts}
			id := th.createSession("root/m1")

			resp, data := th.do("POST", "/session/"+id+"/prompt_async", map[string]any{
				"parts": []map[string]string{{"type": "text", "text": "hold the run slot"}},
			})
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
			}
			<-blocker.started

			path := c.Path
			if path == "/session/{id}" || len(path) > len("/session/{id}") && path[:len("/session/{id}")] == "/session/{id}" {
				path = "/session/" + id + path[len("/session/{id}"):]
			}
			resp, data = th.do(c.Method, path, bodies[c.Name])

			conflict := resp.StatusCode == http.StatusConflict
			if *c.AvailableDuringTask && conflict {
				t.Errorf("%s %s returned 409 while a turn ran, but available_during_task is true: %s",
					c.Method, path, data)
			}
			if !*c.AvailableDuringTask && !conflict {
				t.Errorf("%s %s returned %d while a turn ran, but available_during_task is false — the flag warns of a conflict the route does not raise: %s",
					c.Method, path, resp.StatusCode, data)
			}
		})
	}
	if control == 0 {
		t.Fatal("GET /commands returned no control commands; the check ran against nothing")
	}
}
