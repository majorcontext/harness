package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestProviderReceivesPreviewNotFullText is the load-bearing proof for
// tool-result handles: it drives a real Session.Prompt against a fake
// provider whose first turn calls a tool producing far more than the inline
// limit, then asserts against the SECOND request that provider actually
// received — the one carrying the tool result back to the model — that it
// contains the handle and does NOT contain the retained bytes.
//
// Asserting on the provider's captured *provider.Request, rather than on
// s.History(), is what makes this a proof rather than a restatement of the
// retention unit test: history and the wire could in principle diverge (a
// transcoder could re-expand, a normalization pass could reorder), and the
// question this feature has to answer is what the MODEL is billed for. The
// canonical request is the last engine-owned artifact before a provider
// adapter transcodes it, and every adapter builds its payload from these
// exact parts.
func TestProviderReceivesPreviewNotFullText(t *testing.T) {
	dir := t.TempDir()

	// A distinctive needle that appears ONLY deep inside the retained
	// region, past the inline limit — so finding it anywhere in request 2
	// proves the full text leaked, and not merely that some prefix did.
	const needle = "NEEDLE-THAT-MUST-NOT-REACH-THE-PROVIDER"
	var b strings.Builder
	b.WriteString(strings.Repeat("filler-line-of-output\n", 4000)) // ~88 KB
	b.WriteString(needle + "\n")
	b.WriteString(strings.Repeat("more-filler\n", 4000))
	big := b.String()

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("tc1", "bigtool", `{}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}

	cfg := retainCfg(dir, prov, 2048, 0)
	cfg.Tools = []Tool{bigOutputTool("bigtool", big)}

	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "run the big tool"); err != nil {
		t.Fatal(err)
	}

	if len(prov.requests) != 2 {
		t.Fatalf("provider saw %d requests, want 2", len(prov.requests))
	}
	req := prov.requests[1]

	// Serialize the whole request the way a transcoder would walk it: every
	// message, every part. Anything the model can be billed for is in here.
	wire, err := json.Marshal(req.Messages)
	if err != nil {
		t.Fatal(err)
	}
	payload := string(wire)

	if strings.Contains(payload, needle) {
		t.Errorf("request 2 carries the retained needle — the full tool output reached the provider")
	}
	if !strings.Contains(payload, "handle=trh_1") {
		t.Errorf("request 2 does not carry the retention handle:\n%s", head(payload, 800))
	}
	if !strings.Contains(payload, "read_tool_result") {
		t.Errorf("request 2 does not tell the model how to read the rest:\n%s", head(payload, 800))
	}

	// Size proof, independent of the needle: the request must be a small
	// multiple of the inline limit, not a small multiple of the full
	// output. Retention exists to keep the request small; a payload near
	// len(big) would mean it did nothing even if the needle happened to be
	// cut off.
	if len(payload) > 16*1024 {
		t.Errorf("request 2 payload = %d bytes, want well under the %d-byte tool output", len(payload), len(big))
	}

	// And the bytes are still recoverable: the model can read the needle
	// back through the handle, which is the half that makes retention
	// different from truncation.
	out, err := runReadToolResult(s, json.RawMessage(`{"handle":"trh_1","search":"NEEDLE-THAT-MUST-NOT-REACH-THE-PROVIDER"}`))
	if err != nil {
		t.Fatalf("read_tool_result: %v", err)
	}
	if !strings.Contains(out.Text(), needle) {
		t.Errorf("the retained needle is not recoverable through the handle:\n%s", out.Text())
	}
}

// TestProviderReceivesFullTextWhenRetentionDisabled is the control for the
// test above: with retention off, the same needle DOES reach the provider.
// Without this, the assertion above could pass for the wrong reason (a
// scripted provider that never sees tool results at all).
func TestProviderReceivesFullTextWhenRetentionDisabled(t *testing.T) {
	const needle = "NEEDLE-THAT-MUST-NOT-REACH-THE-PROVIDER"
	big := strings.Repeat("filler-line-of-output\n", 4000) + needle + "\n"

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("tc1", "bigtool", `{}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	cfg := retainCfg(t.TempDir(), prov, 0, 0) // retention disabled
	cfg.Tools = []Tool{bigOutputTool("bigtool", big)}

	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if len(prov.requests) != 2 {
		t.Fatalf("provider saw %d requests, want 2", len(prov.requests))
	}
	wire, err := json.Marshal(prov.requests[1].Messages)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), needle) {
		t.Error("control failed: with retention disabled the full output should reach the provider")
	}
	_ = s
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
