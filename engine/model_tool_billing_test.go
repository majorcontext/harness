package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
	"github.com/majorcontext/harness/provider/openai"
)

// runModelToolRaw runs the model tool's Run function directly against s and
// returns the raw JSON text of a successful result, t.Fatal on a tool error.
// Unlike runModelToolAction/runModelToolListAction (model_tool_test.go), this
// does not decode into modelToolResult/modelListResult — it lets a caller
// decode into its own shape, so a test can assert on wire fields the
// package's exported-for-tests result types do not (yet) carry.
func runModelToolRaw(t *testing.T, s *Session, args string) string {
	t.Helper()
	tool, ok := s.tools[modelToolName]
	if !ok {
		t.Fatal("model tool absent")
	}
	parts, err := tool.Run(context.Background(), s, []byte(args))
	if err != nil {
		t.Fatalf("model tool run(%s): %v", args, err)
	}
	text, ok := parts[0].(*message.Text)
	if !ok {
		t.Fatalf("model tool result is not text: %#v", parts[0])
	}
	return text.Text
}

// providerWireEntry is the wire shape this test expects each entry of the
// model tool's "providers" array to carry: a name plus a billing
// classification. It is deliberately a test-local type, not
// engine.providerInfo, so this test proves the WIRE contract (what an
// agent parsing the tool's JSON response actually sees) rather than
// restating whatever internal type the implementation happens to use.
type providerWireEntry struct {
	Name    string `json:"name"`
	Billing string `json:"billing"`
}

// TestModelToolListLabelsProviderBilling names the gap PR #778's boxes
// system-prompt sentence ("prefer a subscription-backed model over an
// API-billed model") exposed: the model tool's list/status output named
// providers as bare strings ("claude-code", "codex", "anthropic", ...)
// with no field telling the agent which of those strings is
// subscription-backed and which is API-billed, so the sentence gave the
// agent an instruction it had no way to act on from its own tool surface.
//
// Input: a session with three configured providers — "claude-code" (the
// delegated Claude Code CLI backend, ClaudeCodeProviderFamily, always
// subscription-authenticated), "codex" (the ChatGPT Codex backend
// convention, provider/openai.CodexFamily, billed against a ChatGPT
// subscription), and "anthropic" (a plain HTTP adapter authenticated with
// an API key).
//
// Wrong output before this change: each providers[] entry is a bare
// string ("claude-code", not {"name":"claude-code","billing":...}), so
// json.Unmarshal into providerWireEntry fails outright — there is no
// "billing" field on the wire at all. Red-verify: this test fails to
// unmarshal against pre-change model_tool.go for exactly that reason.
func TestModelToolListLabelsProviderBilling(t *testing.T) {
	s := NewSession(Config{
		ModelTool: true,
		Model:     message.ModelRef{Provider: "claude-code", Model: "sonnet"},
		Providers: provider.Registry{
			"claude-code": &scriptedProvider{name: "claude-code"},
			"codex":       &scriptedProvider{name: "codex"},
			"anthropic":   &scriptedProvider{name: "anthropic"},
		},
	})

	raw := runModelToolRaw(t, s, `{"action":"list"}`)

	var wire struct {
		Providers []providerWireEntry `json:"providers"`
	}
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		t.Fatalf("decode providers[] with a billing field: %v (raw=%s)", err, raw)
	}

	got := map[string]string{}
	for _, p := range wire.Providers {
		if p.Billing == "" {
			t.Fatalf("provider %q has an empty billing classification: %s", p.Name, raw)
		}
		got[p.Name] = p.Billing
	}

	want := map[string]string{
		"claude-code": "subscription",
		"codex":       "subscription",
		"anthropic":   "api",
	}
	for name, wantBilling := range want {
		if got[name] != wantBilling {
			t.Errorf("provider %q billing = %q, want %q (raw=%s)", name, got[name], wantBilling, raw)
		}
	}
}

// TestModelToolStatusLabelsProviderBilling is status's sibling of
// TestModelToolListLabelsProviderBilling: status's providers[] must carry
// the identical billing classification list already carries (both are
// backed by configuredProviderInfos(), see modelToolStatus/modelToolList),
// never a second, independently-drifting data source.
func TestModelToolStatusLabelsProviderBilling(t *testing.T) {
	s := NewSession(Config{
		ModelTool: true,
		Model:     message.ModelRef{Provider: "claude-code", Model: "sonnet"},
		Providers: provider.Registry{
			"claude-code": &scriptedProvider{name: "claude-code"},
			"anthropic":   &scriptedProvider{name: "anthropic"},
		},
	})

	raw := runModelToolRaw(t, s, `{"action":"status"}`)

	var wire struct {
		Providers []providerWireEntry `json:"providers"`
	}
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		t.Fatalf("decode providers[] with a billing field: %v (raw=%s)", err, raw)
	}

	got := map[string]string{}
	for _, p := range wire.Providers {
		got[p.Name] = p.Billing
	}
	want := map[string]string{"claude-code": "subscription", "anthropic": "api"}
	for name, wantBilling := range want {
		if got[name] != wantBilling {
			t.Errorf("provider %q billing = %q, want %q (raw=%s)", name, got[name], wantBilling, raw)
		}
	}
}

// TestCodexProviderFamilyMatchesOpenAIPackage pins codexProviderFamily
// against provider/openai.CodexFamily, the same cross-package parity
// precedent TestClaudeCodeProviderFamilyMatchesModelmeta already
// establishes for ClaudeCodeProviderFamily: this package cannot import
// provider/openai for one string (see codexProviderFamily's own doc
// comment), so the literal is duplicated here instead — a test, not the
// type system, is what keeps the duplicate from drifting silently. Red-
// verify: change either constant alone and this test fails.
func TestCodexProviderFamilyMatchesOpenAIPackage(t *testing.T) {
	if codexProviderFamily != openai.CodexFamily {
		t.Fatalf("engine.codexProviderFamily = %q, provider/openai.CodexFamily = %q — these must match", codexProviderFamily, openai.CodexFamily)
	}
}
