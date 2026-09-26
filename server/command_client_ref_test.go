package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
)

type commandClientRefResponse struct {
	Status  string `json:"status"`
	Command struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		ClientRef string `json:"client_ref"`
	} `json:"command"`
}

func TestClientRefCarriesOnTypedCommand(t *testing.T) {
	const clientRef = "pd_01abc"
	parts := []map[string]string{{"type": "text", "text": "/status"}}

	tests := []struct {
		name string
		path string
		body map[string]any
	}{
		{"prompt_async", "prompt_async", map[string]any{
			"parts": parts, "source": "typed", "client_ref": clientRef,
		}},
		{"enqueue", "enqueue", map[string]any{
			"parts": parts, "source": "typed", "seq": 1, "client_ref": clientRef,
		}},
		{"send", "send", map[string]any{
			"parts": parts, "source": "typed", "client_ref": clientRef,
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, &scriptedProvider{name: "test"})
			id := h.createSession("test/m1")
			sse := h.openSSE("?from=0", "")

			resp, data := h.do("POST", "/session/"+id+"/"+tc.path, tc.body)
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("%s status %d: %s", tc.path, resp.StatusCode, data)
			}
			var got commandClientRefResponse
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("decode response: %v (%s)", err, data)
			}
			if got.Status != "command" {
				t.Fatalf("response status = %q, want command", got.Status)
			}
			if got.Command.ClientRef != clientRef {
				t.Errorf("receipt client_ref = %q, want %q", got.Command.ClientRef, clientRef)
			}

			accepted := sse.waitFor(t, "command")
			if accepted.Command == nil || accepted.Command.ClientRef != clientRef {
				t.Fatalf("accepted command event = %+v, want client_ref %q", accepted.Command, clientRef)
			}
			terminal := sse.waitFor(t, "command")
			if terminal.Command == nil || terminal.Command.ClientRef != clientRef {
				t.Fatalf("terminal command event = %+v, want client_ref %q", terminal.Command, clientRef)
			}

			bootstrap := getTranscriptCommands(t, h, id, "")
			if len(bootstrap.Commands) != 1 || bootstrap.Commands[0].ClientRef != clientRef {
				t.Fatalf("bootstrap commands = %+v, want one record with client_ref %q", bootstrap.Commands, clientRef)
			}

			windowed := getPageCommands(t, h, id, "?limit=10")
			if len(windowed.Commands) != 1 || windowed.Commands[0].ClientRef != clientRef {
				t.Fatalf("windowed page commands = %+v, want one record with client_ref %q", windowed.Commands, clientRef)
			}
		})
	}
}

func TestBootMarksAcceptedCommandInterruptedCarriesClientRef(t *testing.T) {
	const clientRef = "pd_enqueued"
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test"}

	srv1 := newServer(t, dir, prov, 0)
	id := createSessionDirect(t, srv1, "test/m1")

	srv1.mu.Lock()
	sess := srv1.sessions[id].sess
	srv1.mu.Unlock()

	cmdID := engine.NewCommandID()
	if _, err := sess.RecordCommandDurable(message.CommandRecord{
		ID:        cmdID,
		Line:      "/compact",
		Name:      "compact",
		Source:    message.PromptSourceTyped,
		Status:    message.CommandAccepted,
		ClientRef: clientRef,
	}, 1); err != nil {
		t.Fatalf("RecordCommandDurable: %v", err)
	}
	if err := srv1.Close(); err != nil {
		t.Fatalf("closing first server: %v", err)
	}

	srv2 := newServer(t, dir, prov, 0)
	srv2Closed := false
	t.Cleanup(func() {
		if !srv2Closed {
			if err := srv2.Close(); err != nil {
				t.Errorf("closing second server: %v", err)
			}
		}
	})
	events := commandEventsForSession(srv2, id)
	if len(events) != 2 {
		t.Fatalf("command events after boot = %d, want 2 (accepted, then interrupted): %+v", len(events), events)
	}
	if events[0].Command == nil || events[0].Command.ClientRef != clientRef {
		t.Fatalf("accepted event = %+v, want client_ref %q", events[0].Command, clientRef)
	}
	interrupted := events[1]
	if interrupted.Command == nil || interrupted.Command.Status != message.CommandInterrupted {
		t.Fatalf("second command event = %+v, want status=interrupted", interrupted.Command)
	}
	if interrupted.Command.ClientRef != clientRef {
		t.Errorf("interrupted event client_ref = %q, want %q", interrupted.Command.ClientRef, clientRef)
	}
	if want := "harness restarted before /compact finished; it will not run again"; interrupted.Command.Text != want {
		t.Errorf("interrupted text = %q, want %q", interrupted.Command.Text, want)
	}

	reloaded, err := srv2.opts.LoadSession(id)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	cmds := reloaded.Commands()
	if len(cmds) != 1 || cmds[0].ClientRef != clientRef {
		t.Fatalf("reloaded Commands() = %+v, want one record with client_ref %q", cmds, clientRef)
	}
	if err := srv2.Close(); err != nil {
		t.Fatal(err)
	}
	srv2Closed = true
	srv3 := newServer(t, dir, prov, 0)
	t.Cleanup(func() { srv3.Close() })
	if events := commandEventsForSession(srv3, id); len(events) != 2 {
		t.Fatalf("second boot added events: %+v", events)
	}
}

func assertClientRefNowhere(t *testing.T, h *harness, id, clientRef string) {
	t.Helper()
	sessionLog, err := os.ReadFile(filepath.Join(h.dir, id+".jsonl"))
	if err != nil {
		t.Fatalf("read session log: %v", err)
	}
	if strings.Contains(string(sessionLog), clientRef) {
		t.Errorf("session log contains %q, want no occurrence", clientRef)
	}

	eventsLog, err := os.ReadFile(filepath.Join(h.dir, journalName))
	if err != nil {
		t.Fatalf("read server events log: %v", err)
	}
	if strings.Contains(string(eventsLog), clientRef) {
		t.Errorf("server events log contains %q, want no occurrence", clientRef)
	}
}

func TestClientRefDroppedForOrdinaryPrompt(t *testing.T) {
	const clientRef = "pd_ordinary"
	parts := []map[string]string{{"type": "text", "text": "hello"}}

	routes := []struct {
		name string
		path string
		body map[string]any
	}{
		{"prompt_async", "prompt_async", map[string]any{"parts": parts, "client_ref": clientRef}},
		{"enqueue", "enqueue", map[string]any{"parts": parts, "seq": 1, "client_ref": clientRef}},
		{"send", "send", map[string]any{"parts": parts, "client_ref": clientRef}},
	}

	for _, tc := range routes {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, newCapturingProvider(asstTurn("ok")))
			id := h.createSession("test/m1")

			resp, data := h.do("POST", "/session/"+id+"/"+tc.path, tc.body)
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("%s status %d: %s", tc.path, resp.StatusCode, data)
			}
			h.waitIdle(id)

			users := h.userMessages(id)
			if len(users) != 1 || users[0].Parts.Text() != "hello" {
				t.Fatalf("user messages = %+v, want one with text hello", users)
			}
			assertClientRefNowhere(t, h, id, clientRef)
		})
	}

	t.Run("queued_behind_busy_turn", func(t *testing.T) {
		prov := newBlockingProvider("test")
		h := newHarness(t, prov)
		t.Cleanup(prov.releaseAll)
		id := h.createSession("test/m1")

		resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
			"parts": []map[string]string{{"type": "text", "text": "occupy"}},
		})
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("occupy prompt_async status %d: %s", resp.StatusCode, data)
		}
		<-prov.started // the run slot is now held; the next prompt must queue

		resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{"parts": parts, "client_ref": clientRef})
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("queued prompt_async status %d: %s", resp.StatusCode, data)
		}
		var got promptAsyncResponse
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("decode response: %v (%s)", err, data)
		}
		if got.Status != "queued" {
			t.Fatalf("second prompt status = %q, want queued (the first is still running)", got.Status)
		}

		prov.releaseAll()
		h.waitIdle(id)

		users := h.userMessages(id)
		if len(users) != 2 {
			t.Fatalf("user messages = %+v, want two (occupy, then hello)", users)
		}
		assertClientRefNowhere(t, h, id, clientRef)
	})
}

func TestClientRefValidation(t *testing.T) {
	invalid := map[string]string{
		"too_long":  strings.Repeat("a", 129),
		"non_ascii": "pd_\xc3\x28",
	}
	routes := []string{"prompt_async", "enqueue", "send"}

	for _, path := range routes {
		for name, ref := range invalid {
			t.Run(path+"/"+name, func(t *testing.T) {
				h := newHarness(t, &scriptedProvider{name: "test"})
				id := h.createSession("test/m1")

				body := map[string]any{
					"parts":      []map[string]string{{"type": "text", "text": "/status"}},
					"source":     "typed",
					"client_ref": ref,
				}
				if path == "enqueue" {
					body["seq"] = 1
				}
				resp, data := h.do("POST", "/session/"+id+"/"+path, body)
				if resp.StatusCode != http.StatusBadRequest {
					t.Fatalf("%s status %d, want 400: %s", path, resp.StatusCode, data)
				}
				if !strings.Contains(string(data), "client_ref") {
					t.Errorf("error body = %s, want it to name client_ref", data)
				}

				if users := h.userMessages(id); len(users) != 0 {
					t.Errorf("user messages = %+v, want none", users)
				}
				if cmds := h.sessionDirect(id).Commands(); len(cmds) != 0 {
					t.Errorf("Commands() = %+v, want none", cmds)
				}

				if path != "enqueue" {
					return
				}
				resp, data = h.do("POST", "/session/"+id+"/enqueue", map[string]any{
					"parts":      []map[string]string{{"type": "text", "text": "/status"}},
					"source":     "typed",
					"seq":        1,
					"client_ref": "pd_retry",
				})
				if resp.StatusCode != http.StatusAccepted {
					t.Fatalf("retry enqueue status %d: %s", resp.StatusCode, data)
				}
				var got commandClientRefResponse
				if err := json.Unmarshal(data, &got); err != nil {
					t.Fatalf("decode retry response: %v (%s)", err, data)
				}
				if got.Status != "command" {
					t.Fatalf("retry enqueue status = %q, want command (not duplicate)", got.Status)
				}
				if got.Command.ClientRef != "pd_retry" {
					t.Errorf("retry receipt client_ref = %q, want pd_retry", got.Command.ClientRef)
				}
			})
		}
	}
}
