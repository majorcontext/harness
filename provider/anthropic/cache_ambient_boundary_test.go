package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/majorcontext/harness/message"
)

// Cross-turn prompt-cache reuse over the ambient-status boundary.
//
// withAmbientStatus (engine/process.go) appends an *EngineContext part to the
// newest user message on the per-request copy only, so the message that
// carried it on turn N is re-rendered without it on turn N+1. These tests
// drive the real transcoder for a turn PAIR and ask the only question the
// cache asks: are the bytes some breakpoint on turn N covered still a prefix
// of turn N+1's request? See markAmbientBoundary.

// renderBlock serializes one block with its cache_control STRIPPED. The
// marker moves forward every request by design and is not itself an
// invalidator, so a diff that kept it would report a divergence on any pair.
func renderBlock(t *testing.T, role string, b apiBlock) string {
	t.Helper()
	b.CacheControl = nil
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return role + "|" + string(raw)
}

// blocks renders every message block in order. marked reports, for each
// index, whether that block carries a cache_control breakpoint.
func blocks(t *testing.T, out *apiRequest) (flat []string, marked []bool) {
	t.Helper()
	for _, m := range out.Messages {
		for _, b := range m.Content {
			flat = append(flat, renderBlock(t, m.Role, b))
			marked = append(marked, b.CacheControl != nil)
		}
	}
	return flat, marked
}

// reusablePrefixLen returns the length of the LONGEST prefix of turnN that
// both ends on a breakpoint and is still a prefix of turnNext — the largest
// entry turn N+1 can actually read back. Zero means every entry turn N wrote
// is unreadable on turn N+1.
func reusablePrefixLen(t *testing.T, turnN, turnNext *apiRequest) int {
	t.Helper()
	flat, marked := blocks(t, turnN)
	next, _ := blocks(t, turnNext)
	best := 0
	for i, isMarked := range marked {
		if !isMarked || i >= len(next) {
			continue
		}
		ok := true
		for j := 0; j <= i; j++ {
			if flat[j] != next[j] {
				ok = false
				break
			}
		}
		if ok {
			best = i + 1
		}
	}
	return best
}

func userMsg(text string, parts ...message.Part) message.Message {
	p := message.Parts{&message.Text{Text: text}}
	p = append(p, parts...)
	return message.Message{Role: message.RoleUser, Parts: p}
}

func assistantMsg(text string) message.Message {
	return message.Message{Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: text}}}
}

func ambient() *message.EngineContext {
	return &message.EngineContext{Text: "[engine: harness v1 · session_sync=fsync · engine started 2026-09-02T17:00:00Z]"}
}

// nextTurn is the turn N+1 request every case below is measured against: the
// same history with turn N's user message rendered WITHOUT its ambient part,
// turn N's answer, and a new user message carrying this turn's ambient part.
func nextTurn(t *testing.T) *apiRequest {
	t.Helper()
	out, err := transcodeRequest(baseRequest(
		userMsg("u1"),
		assistantMsg("a1"),
		userMsg("u2"),
		assistantMsg("a2"),
		userMsg("u3", ambient()),
	), CacheTTL1h)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestAmbientBoundaryKeepsCrossTurnCachePrefix: the first request of turn N
// writes an entry covering everything up to the ambient block (u1, a1, u2),
// and turn N+1 reads it. Without markAmbientBoundary the only messages-tier
// entry ends AFTER the ambient block and nothing is reusable.
func TestAmbientBoundaryKeepsCrossTurnCachePrefix(t *testing.T) {
	turnN, err := transcodeRequest(baseRequest(
		userMsg("u1"),
		assistantMsg("a1"),
		userMsg("u2", ambient()),
	), CacheTTL1h)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := reusablePrefixLen(t, turnN, nextTurn(t)), 3; got != want {
		t.Errorf("reusable prefix = %d blocks, want %d (u1, a1, u2)", got, want)
	}
}

// TestAmbientBoundaryHoldsMidTurn: a later step of the same turn appends the
// assistant reply and tool traffic AFTER the ambient-bearing message. The
// boundary stays before the ambient block, so this request too writes an
// entry turn N+1 can read.
func TestAmbientBoundaryHoldsMidTurn(t *testing.T) {
	turnN, err := transcodeRequest(baseRequest(
		userMsg("u1"),
		assistantMsg("a1"),
		userMsg("u2", ambient()),
		assistantMsg("thinking about it"),
		userMsg("tool output"),
	), CacheTTL1h)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := reusablePrefixLen(t, turnN, nextTurn(t)), 3; got != want {
		t.Errorf("reusable prefix = %d blocks, want %d (u1, a1, u2)", got, want)
	}
}

// TestNoAmbientNeedsNoBoundary: with no ambient part the tail breakpoint is
// already reusable, and no extra breakpoint is spent.
func TestNoAmbientNeedsNoBoundary(t *testing.T) {
	turnN, err := transcodeRequest(baseRequest(
		userMsg("u1"),
		assistantMsg("a1"),
		userMsg("u2"),
	), CacheTTL1h)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := reusablePrefixLen(t, turnN, nextTurn(t)), 3; got != want {
		t.Errorf("reusable prefix = %d blocks, want %d", got, want)
	}
	_, marked := blocks(t, turnN)
	count := 0
	for _, m := range marked {
		if m {
			count++
		}
	}
	if count != 1 {
		t.Errorf("messages-tier breakpoints = %d, want 1 (tail only)", count)
	}
}

// TestAmbientBoundaryStaysWithinBreakpointBudget: the API allows four
// cache_control breakpoints per request. A request carrying several ambient
// segments spends three at most — final system block, ambient boundary, tail.
func TestAmbientBoundaryStaysWithinBreakpointBudget(t *testing.T) {
	out, err := transcodeRequest(baseRequest(
		userMsg("u1"),
		assistantMsg("a1"),
		userMsg("u2", ambient(), ambient(), ambient()),
	), CacheTTL1h)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, s := range out.System {
		if s.CacheControl != nil {
			count++
		}
	}
	_, marked := blocks(t, out)
	for _, m := range marked {
		if m {
			count++
		}
	}
	if count > 4 {
		t.Errorf("cache_control breakpoints = %d, want <= 4", count)
	}
	if count != 3 {
		t.Errorf("cache_control breakpoints = %d, want 3 (system, boundary, tail)", count)
	}
}

// TestAmbientFirstBlockNeedsNoBoundary: an ambient block with nothing before
// it has no boundary to mark, and must not crash or mark the block itself.
func TestAmbientFirstBlockNeedsNoBoundary(t *testing.T) {
	out, err := transcodeRequest(baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{ambient(), &message.Text{Text: "u1"}}},
	), CacheTTL1h)
	if err != nil {
		t.Fatal(err)
	}
	_, marked := blocks(t, out)
	if marked[0] {
		t.Error("leading ambient block carries a breakpoint, want none")
	}
	count := 0
	for _, m := range marked {
		if m {
			count++
		}
	}
	if count != 1 {
		t.Errorf("messages-tier breakpoints = %d, want 1 (tail only)", count)
	}
}
