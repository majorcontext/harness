//go:build live

// Live probe for one question the repository could previously only assume:
// is a Codex Responses response ID usable from a DIFFERENT websocket
// connection than the one that produced it?
//
// provider/AGENTS.md keeps lineage "keyed by the session pool entry" and
// wsPool.invalidate drops lineage with the socket, so an idle or aged
// connection costs a full history re-send. That cost is only unavoidable if
// the server's response state is genuinely CONNECTION-scoped. This test
// asks the real backend.
//
// The experiment is a three-step controlled comparison on one account:
//
//  1. connection A, full request              -> response ID
//  2. connection B, freshly dialed, chained on that ID
//  3. connection A, still open, chained on that SAME ID   (the control)
//
// Step 3 proves the ID itself is live and the request shape is chainable.
// Step 2 then isolates the single changed variable: the connection.
//
// Run (needs a box whose egress proxy injects a real Codex credential):
//
//	HARNESS_LIVE=1 go test -tags live -run TestCodexChain -v ./provider/openai/
//
// Env:
//
//	HARNESS_LIVE=1        required, or the test skips
//	CODEX_API_KEY         bearer to send; defaults to $CODEX_DUMMY_KEY
//	CODEX_BASE_URL        defaults to https://chatgpt.com/backend-api/codex
//	CODEX_MODEL           defaults to gpt-5.6-sol
package openai

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// liveCodexClient builds a Client configured exactly like a box's "codex"
// provider entry, or skips.
func liveCodexClient(t *testing.T) (*Client, message.ModelRef) {
	t.Helper()
	if os.Getenv("HARNESS_LIVE") == "" {
		t.Skip("HARNESS_LIVE unset; skipping live Codex websocket probe")
	}
	key := os.Getenv("CODEX_API_KEY")
	if key == "" {
		key = os.Getenv("CODEX_DUMMY_KEY")
	}
	if key == "" {
		t.Skip("no CODEX_API_KEY or CODEX_DUMMY_KEY; skipping live Codex websocket probe")
	}
	base := os.Getenv("CODEX_BASE_URL")
	if base == "" {
		base = "https://chatgpt.com/backend-api/codex"
	}
	model := os.Getenv("CODEX_MODEL")
	if model == "" {
		model = "gpt-5.6-sol"
	}
	return &Client{
		APIKey:                key,
		BaseURL:               base,
		ResponsesPath:         "/responses",
		Family:                CodexFamily,
		OmitResponseParams:    []string{"max_output_tokens", "temperature", "top_p", "metadata"},
		SanitizeToolSchemas:   true,
		UseWebSocketTransport: true,
	}, message.ModelRef{Provider: "codex", Model: model}
}

// liveDial opens one Codex Responses websocket through the production dial.
func liveDial(t *testing.T, ctx context.Context, prepared *preparedRequest) *websocket.Conn {
	t.Helper()
	conn, _, err := dialResponsesWebSocket(ctx, prepared.url, prepared.headers, prepared.client, wsDefaultConnectTimeout)
	if err != nil {
		t.Fatalf("dialResponsesWebSocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	return conn
}

// liveDrain reads frames until a terminal one and reports the completed
// response ID, the terminal frame name, and the terminal frame bytes.
func liveDrain(t *testing.T, ctx context.Context, conn *websocket.Conn) (responseID, terminal string, data []byte) {
	t.Helper()
	for {
		name, frame, err := readFirstFrame(ctx, conn, wsDefaultIdleTimeout)
		if err != nil {
			t.Fatalf("readFirstFrame: %v", err)
		}
		var ev struct {
			Response struct {
				ID string `json:"id"`
			} `json:"response"`
		}
		if json.Unmarshal(frame, &ev) == nil && ev.Response.ID != "" {
			responseID = ev.Response.ID
		}
		if isWSTerminalEvent(name) {
			return responseID, name, frame
		}
	}
}

// TestCodexChainAcrossRedialLive pins the named failure this repository's
// idle and age timeouts exist for: a Codex response ID is rejected on any
// connection other than the one that produced it, so a dropped pooled
// connection genuinely takes its lineage with it.
//
// If the backend ever stops behaving this way, THIS TEST FAILS, and the fix
// is to chain across a re-dial and delete both timeouts' lineage cost.
func TestCodexChainAcrossRedialLive(t *testing.T) {
	client, model := liveCodexClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	first := &provider.Request{
		Model:      model,
		System:     []string{"Answer with one lowercase word and nothing else."},
		Messages:   []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "Say: one"}}}},
		SessionKey: "harness-live-redial-probe",
	}
	prepared, err := client.prepareRequest(first, false)
	if err != nil {
		t.Fatalf("prepareRequest: %v", err)
	}

	connA := liveDial(t, ctx, prepared)
	if err := sendResponseCreate(ctx, connA, prepared.body); err != nil {
		t.Fatalf("sendResponseCreate on connection A: %v", err)
	}
	responseID, terminal, frame := liveDrain(t, ctx, connA)
	if terminal != "response.completed" {
		t.Fatalf("connection A terminated with %s, want response.completed: %s", terminal, frame)
	}
	if responseID == "" {
		t.Fatal("connection A completed with no response ID")
	}
	t.Logf("step 1: connection A completed a full request (response ID captured, not logged)")

	// The chained follow-up: previous_response_id plus one new input item.
	// Both step 2 and step 3 send these identical bytes.
	suffix := []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"Say: two"}]}`)}
	chained := responseCreateOptions{PreviousResponseID: responseID, Input: suffix, InputSet: true}

	// Step 2: the same ID on a connection that never saw it.
	connB := liveDial(t, ctx, prepared)
	if err := sendResponseCreate(ctx, connB, prepared.body, chained); err != nil {
		t.Fatalf("sendResponseCreate on freshly dialed connection B: %v", err)
	}
	nameB, frameB, errB := readFirstFrame(ctx, connB, wsDefaultIdleTimeout)
	redialChained := false
	switch {
	case errB != nil:
		t.Logf("step 2: fresh connection B first frame failed: %v", errB)
	case isPreviousResponseNotFoundFrame(nameB, frameB):
		t.Logf("step 2: fresh connection B REJECTED the ID as not found (%s)", nameB)
	case nameB == "response.failed" || nameB == "error":
		t.Logf("step 2: fresh connection B failed with %s: %s", nameB, frameB)
	default:
		redialChained = true
		t.Logf("step 2: fresh connection B ACCEPTED the ID (first frame %s)", nameB)
	}

	// Step 3, the control: the same ID and bytes on the connection that
	// produced it. This must work, or step 2 proves nothing.
	if err := sendResponseCreate(ctx, connA, prepared.body, chained); err != nil {
		t.Fatalf("sendResponseCreate on reused connection A: %v", err)
	}
	nameA, frameA, errA := readFirstFrame(ctx, connA, wsDefaultIdleTimeout)
	if errA != nil {
		t.Fatalf("control invalid: reused connection A first frame failed: %v", errA)
	}
	if isPreviousResponseNotFoundFrame(nameA, frameA) || nameA == "response.failed" || nameA == "error" {
		t.Fatalf("control invalid: reused connection A rejected its OWN response ID (%s): %s", nameA, frameA)
	}
	t.Logf("step 3 (control): reused connection A ACCEPTED the ID (first frame %s)", nameA)

	if redialChained {
		t.Fatal("a Codex response ID chained on a FRESHLY DIALED connection: " +
			"response state is no longer connection-scoped, so wsPool must chain " +
			"across a re-dial instead of dropping lineage with the socket")
	}
}

// TestCodexStaleChainMissVocabularyLive records the wire vocabulary the real
// backend uses to reject an unusable previous_response_id on an otherwise
// healthy, reused connection — the exact condition
// isPreviousResponseNotFoundFrame gates chain-miss recovery on.
//
// The named failure: if the rejection carries no "code" field, then
// isNotFoundErrorCode sees "", isPreviousResponseNotFoundFrame reports
// false, and wsPool.stream's recovery never fires. The turn surfaces a plain
// non-retryable provider error instead of re-sending the complete request.
func TestCodexStaleChainMissVocabularyLive(t *testing.T) {
	client, model := liveCodexClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	req := &provider.Request{
		Model:      model,
		System:     []string{"Answer with one lowercase word and nothing else."},
		Messages:   []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "Say: one"}}}},
		SessionKey: "harness-live-chain-miss-probe",
	}
	prepared, err := client.prepareRequest(req, false)
	if err != nil {
		t.Fatalf("prepareRequest: %v", err)
	}

	conn := liveDial(t, ctx, prepared)
	if err := sendResponseCreate(ctx, conn, prepared.body); err != nil {
		t.Fatalf("sendResponseCreate: %v", err)
	}
	if _, terminal, frame := liveDrain(t, ctx, conn); terminal != "response.completed" {
		t.Fatalf("first request terminated with %s, want response.completed: %s", terminal, frame)
	}

	// A well-formed but nonexistent response ID on this now-reused, healthy
	// connection. The connection state is valid; only the reference is not.
	bogus := responseCreateOptions{
		PreviousResponseID: "resp_00000000000000000000000000",
		Input:              []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"Say: two"}]}`)},
		InputSet:           true,
	}
	if err := sendResponseCreate(ctx, conn, prepared.body, bogus); err != nil {
		t.Fatalf("sendResponseCreate with a bogus previous_response_id: %v", err)
	}
	name, frame, err := readFirstFrame(ctx, conn, wsDefaultIdleTimeout)
	if err != nil {
		t.Fatalf("readFirstFrame: %v", err)
	}
	recognized := isPreviousResponseNotFoundFrame(name, frame)
	t.Logf("stale-reference rejection: frame=%s recognized_as_chain_miss=%v body=%s", name, recognized, frame)
	if !recognized {
		t.Errorf("isPreviousResponseNotFoundFrame did not recognize the real backend's "+
			"stale previous_response_id rejection (frame %s), so chain-miss recovery cannot fire", name)
	}
}

// TestCodexIdleToleranceLive measures how long a pooled Codex Responses
// websocket can sit with NO traffic and still accept a chained request.
// This transport sends no keepalive ping, so the answer bounds any useful
// value of wsPool.idleTimeout: a reuse window wider than the server's (or
// an intermediary's) own idle tolerance only buys a failed send plus an
// HTTP fallback, which loses the lineage anyway.
//
// Real elapsed time is the independent variable here, so this probe waits
// on a real clock. That is why it is live-tagged and never runs in the
// ordinary suite, which forbids sleeping tests.
//
// Set CODEX_IDLE_GAPS to a comma-separated Go duration list (default
// "7m,20m"). Each gap is measured from the previous response's completion.
func TestCodexIdleToleranceLive(t *testing.T) {
	client, model := liveCodexClient(t)
	gaps := []time.Duration{7 * time.Minute, 20 * time.Minute}
	if spec := os.Getenv("CODEX_IDLE_GAPS"); spec != "" {
		gaps = nil
		for _, field := range strings.Split(spec, ",") {
			d, err := time.ParseDuration(strings.TrimSpace(field))
			if err != nil {
				t.Fatalf("CODEX_IDLE_GAPS %q: %v", field, err)
			}
			gaps = append(gaps, d)
		}
	}
	var budget time.Duration
	for _, g := range gaps {
		budget += g
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget+5*time.Minute)
	defer cancel()

	req := &provider.Request{
		Model:      model,
		System:     []string{"Answer with one lowercase word and nothing else."},
		Messages:   []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "Say: one"}}}},
		SessionKey: "harness-live-idle-tolerance-probe",
	}
	prepared, err := client.prepareRequest(req, false)
	if err != nil {
		t.Fatalf("prepareRequest: %v", err)
	}

	conn := liveDial(t, ctx, prepared)
	if err := sendResponseCreate(ctx, conn, prepared.body); err != nil {
		t.Fatalf("sendResponseCreate: %v", err)
	}
	responseID, terminal, frame := liveDrain(t, ctx, conn)
	if terminal != "response.completed" || responseID == "" {
		t.Fatalf("first request terminated with %s (id set: %v): %s", terminal, responseID != "", frame)
	}
	t.Logf("baseline: lineage established on a live connection")

	for _, gap := range gaps {
		timer := time.NewTimer(gap)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			t.Fatalf("probe context ended while waiting %s: %v", gap, ctx.Err())
		}
		timer.Stop()

		chained := responseCreateOptions{
			PreviousResponseID: responseID,
			Input:              []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"Say: two"}]}`)},
			InputSet:           true,
		}
		if err := sendResponseCreate(ctx, conn, prepared.body, chained); err != nil {
			t.Fatalf("IDLE TOLERANCE < %s: send failed after %s idle: %v", gap, gap, err)
		}
		id, name, body := liveDrain(t, ctx, conn)
		if name != "response.completed" {
			t.Fatalf("IDLE TOLERANCE < %s: chained request after %s idle terminated with %s: %s", gap, gap, name, body)
		}
		if id != "" {
			responseID = id
		}
		t.Logf("IDLE TOLERANCE >= %s: a chained request succeeded after %s of no traffic", gap, gap)
	}
}

// TestCodexIdleKeepaliveLive tests what, if anything, keeps an idle pooled
// Codex connection alive. TestCodexIdleToleranceLive measured the bare
// pool's behavior: a chained request succeeds after 60s of no traffic and
// fails after 90s, with the server's own close reason "keepalive ping
// timeout". wsPool leaves an idle connection with no reader at all
// (wsFrameSource reads only while a response streams), so this asks
// whether that is the cause.
//
// Two modes, each on its own connection:
//
//	read pump      one goroutine owns every read, so it sits inside
//	               conn.Read for the whole idle gap — where
//	               coder/websocket answers a server ping automatically.
//	pump plus ping the same, and it also sends a client ping on an
//	               interval. coder/websocket's Ping needs a concurrent
//	               reader for its pong, which the pump provides.
//
// The named failure each mode pins: if a mode survives a gap the bare pool
// cannot, that mode is the fix and the reuse window can then be widened. If
// NEITHER survives, no keepalive can work, and the only correct change is
// to shrink the reuse window toward the measured ~60s life.
//
// A read pump is also the only shape a real fix could take: canceling a
// coder/websocket read closes the connection, so an idle-only reader could
// never hand the socket back to the pool.
//
// Measured 2026-09-09, against a bare-pool life of 60-90s:
//
//   - Both modes chained successfully after a 7m gap. A reader alone is
//     therefore enough: coder/websocket answers the server ping from inside
//     Read, which is exactly what the reader-less pool never does.
//   - One earlier read-pump run died at ~75s with a bare "failed to read
//     frame header: EOF" and no close handshake. It did not reproduce. Treat
//     a read pump as the keepalive mechanism, not as a guarantee: the path
//     can still drop, which the pool must handle regardless.
//   - Every client ping was answered on the 30s interval, so a client ping
//     is available as belt-and-braces, but the measurement does not show it
//     is required.
func TestCodexIdleKeepaliveLive(t *testing.T) {
	gap := 7 * time.Minute
	if spec := os.Getenv("CODEX_PUMP_GAP"); spec != "" {
		d, err := time.ParseDuration(spec)
		if err != nil {
			t.Fatalf("CODEX_PUMP_GAP %q: %v", spec, err)
		}
		gap = d
	}
	tests := []struct {
		name         string
		pingInterval time.Duration
	}{
		{name: "read_pump_only"},
		{name: "read_pump_plus_client_ping", pingInterval: 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, model := liveCodexClient(t)
			ctx, cancel := context.WithTimeout(context.Background(), gap+5*time.Minute)
			defer cancel()

			req := &provider.Request{
				Model:      model,
				System:     []string{"Answer with one lowercase word and nothing else."},
				Messages:   []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "Say: one"}}}},
				SessionKey: "harness-live-keepalive-" + tt.name,
			}
			prepared, err := client.prepareRequest(req, false)
			if err != nil {
				t.Fatalf("prepareRequest: %v", err)
			}
			conn := liveDial(t, ctx, prepared)

			frames := make(chan wsFrame, 256)
			pumpErr := make(chan error, 1)
			go func() {
				for {
					name, data, err := readResponsesFrame(ctx, conn)
					if err != nil {
						pumpErr <- err
						close(frames)
						return
					}
					frames <- wsFrame{name: name, data: data}
				}
			}()
			drain := func(what string) string {
				t.Helper()
				var responseID string
				for {
					select {
					case f, ok := <-frames:
						if !ok {
							t.Fatalf("%s: read pump ended: %v", what, <-pumpErr)
						}
						var ev struct {
							Response struct {
								ID string `json:"id"`
							} `json:"response"`
						}
						if json.Unmarshal(f.data, &ev) == nil && ev.Response.ID != "" {
							responseID = ev.Response.ID
						}
						if isWSTerminalEvent(f.name) {
							if f.name != "response.completed" {
								t.Fatalf("%s terminated with %s: %s", what, f.name, f.data)
							}
							return responseID
						}
					case err := <-pumpErr:
						t.Fatalf("%s: read pump failed: %v", what, err)
					case <-ctx.Done():
						t.Fatalf("%s: probe context ended: %v", what, ctx.Err())
					}
				}
			}

			if err := sendResponseCreate(ctx, conn, prepared.body); err != nil {
				t.Fatalf("sendResponseCreate: %v", err)
			}
			responseID := drain("baseline request")
			if responseID == "" {
				t.Fatal("baseline request completed with no response ID")
			}

			// A zero interval leaves pings off; the timer then never fires
			// within the gap.
			pingEvery := tt.pingInterval
			if pingEvery <= 0 {
				pingEvery = gap + time.Minute
			}
			ping := time.NewTicker(pingEvery)
			defer ping.Stop()
			deadline := time.NewTimer(gap)
			defer deadline.Stop()
			start := time.Now()
		wait:
			for {
				select {
				case <-deadline.C:
					break wait
				case <-ping.C:
					if err := conn.Ping(ctx); err != nil {
						t.Fatalf("KEEPALIVE FAILED after %s: client ping: %v", time.Since(start).Round(time.Second), err)
					}
					t.Logf("client ping answered at %s idle", time.Since(start).Round(time.Second))
				case err := <-pumpErr:
					t.Fatalf("KEEPALIVE FAILED: connection died after %s idle (target %s): %v", time.Since(start).Round(time.Second), gap, err)
				case <-ctx.Done():
					t.Fatalf("probe context ended while waiting %s: %v", gap, ctx.Err())
				}
			}

			chained := responseCreateOptions{
				PreviousResponseID: responseID,
				Input:              []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"Say: two"}]}`)},
				InputSet:           true,
			}
			if err := sendResponseCreate(ctx, conn, prepared.body, chained); err != nil {
				t.Fatalf("sendResponseCreate after %s idle: %v", gap, err)
			}
			drain("chained request")
			t.Logf("KEEPALIVE WORKS: a chained request succeeded after %s of no request traffic, "+
				"a gap the reader-less pool cannot survive", gap)
		})
	}
}
