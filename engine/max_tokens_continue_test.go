package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/provider"
)

// engineContextTexts returns the text of every *message.EngineContext part
// on the newest RoleUser message in req.Messages, in order. Several ambient
// segments (process/MCP/identity/task-notification/continuation-nudge) can
// stack onto that one message (see withAmbientStatus), so a test asserting
// on one of them must scan every part rather than assume it is the last —
// unlike lastUserText (process_ambient_test.go), which is only safe when
// the caller controls exactly which single segment is present.
func engineContextTexts(req *provider.Request) []string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != message.RoleUser {
			continue
		}
		var texts []string
		for _, p := range req.Messages[i].Parts {
			if ec, ok := p.(*message.EngineContext); ok {
				texts = append(texts, ec.Text)
			}
		}
		return texts
	}
	return nil
}

func containsSubstring(texts []string, substr string) bool {
	for _, t := range texts {
		if strings.Contains(t, substr) {
			return true
		}
	}
	return false
}

// TestMaxTokensWithToolCallAutoContinues reproduces the box
// harness-parallel-tools incident: the provider stops mid-tool-call
// emission with stop reason "max_tokens". Before this fix,
// appendUnexecutedToolCallResults synthesized the usual unexecuted-call
// result and runAgenticLoop returned -- the session then sat idle with no
// further model call, a silent work stoppage on an autonomous fleet. With
// Config.MaxTokensContinuations enabled, the loop must instead issue a
// follow-up model call carrying the synthetic unexecuted-tool-call result
// plus a continuation nudge, in the SAME Prompt call.
func TestMaxTokensWithToolCallAutoContinues(t *testing.T) {
	tc := toolCall("tc1", "bash", `{"command":"echo hi"}`)
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopMaxTokens, tc),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers:              provider.Registry{"test": prov},
		Model:                  message.ModelRef{Provider: "test", Model: "m1"},
		MaxTokensContinuations: 3,
	})

	final, err := s.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt = %v, want success (the loop should auto-continue)", err)
	}
	if final.Parts.Text() != "done" {
		t.Errorf("final = %q, want %q", final.Parts.Text(), "done")
	}
	if len(prov.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2 (the auto-continue must issue a real follow-up model call)", len(prov.requests))
	}

	h := s.History()
	if len(h) != 4 {
		t.Fatalf("history len = %d, want 4 (user, assistant(tool_call), synthetic tool result, assistant(done)): %+v", len(h), h)
	}
	if h[2].Role != message.RoleTool {
		t.Fatalf("h[2].Role = %s, want tool (the synthetic unexecuted-call result)", h[2].Role)
	}
	tr, ok := h[2].Parts[0].(*message.ToolResult)
	if !ok {
		t.Fatalf("h[2].Parts[0] = %T, want *message.ToolResult", h[2].Parts[0])
	}
	if tr.CallID != "tc1" || !tr.IsError {
		t.Errorf("synthetic result = %+v, want CallID=tc1 IsError=true", tr)
	}
	if !strings.Contains(tr.Content.Text(), `"max_tokens"`) {
		t.Errorf("synthetic result text = %q, want it to name the max_tokens stop reason", tr.Content.Text())
	}
	if got := s.toolExecutions(); got != 0 {
		t.Errorf("toolExecutions() = %d, want 0 (a truncated call must never actually run)", got)
	}

	// The follow-up request must carry a continuation nudge naming the
	// attempt and the bound, so the model knows why it is being asked to
	// continue and how much budget remains.
	texts := engineContextTexts(prov.requests[1])
	if !containsSubstring(texts, "[continuation:") {
		t.Errorf("second request ambient segments = %v, want a [continuation: ...] nudge", texts)
	}
	if !containsSubstring(texts, "max_tokens") {
		t.Errorf("second request ambient segments = %v, want the nudge to name max_tokens", texts)
	}
	if !containsSubstring(texts, "1 of 3") {
		t.Errorf("second request ambient segments = %v, want the nudge to report attempt 1 of 3", texts)
	}

	// The nudge must never leak into a THIRD request -- there is none here,
	// but the first request (the original turn) must not have carried it
	// either, since the nudge only exists once a max_tokens continuation has
	// actually been decided.
	if containsSubstring(engineContextTexts(prov.requests[0]), "[continuation:") {
		t.Errorf("first request carried a continuation nudge before any max_tokens stop occurred")
	}
}

// TestMaxTokensPureTextContinuesOnce covers a max_tokens stop with NO tool
// calls at all -- pure text truncation. Unlike Claude Code (which lets the
// turn end and relies on the user re-prompting), an autonomous harness
// session must not require a human, so this must also auto-continue.
func TestMaxTokensPureTextContinuesOnce(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopMaxTokens, &message.Text{Text: "partial output "}),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "rest of output"}),
	}}
	s := NewSession(Config{
		Providers:              provider.Registry{"test": prov},
		Model:                  message.ModelRef{Provider: "test", Model: "m1"},
		MaxTokensContinuations: 3,
	})

	final, err := s.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt = %v, want success", err)
	}
	if final.Parts.Text() != "rest of output" {
		t.Errorf("final = %q, want %q", final.Parts.Text(), "rest of output")
	}
	if len(prov.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2 (a pure-text max_tokens stop must also auto-continue)", len(prov.requests))
	}

	h := s.History()
	if len(h) != 3 {
		t.Fatalf("history len = %d, want 3 (user, assistant(partial), assistant(rest)) -- no synthetic tool message since no ToolCall was ever emitted: %+v", len(h), h)
	}
	if h[1].Role != message.RoleAssistant || h[2].Role != message.RoleAssistant {
		t.Fatalf("h[1]/h[2] roles = %s/%s, want assistant/assistant", h[1].Role, h[2].Role)
	}

	texts := engineContextTexts(prov.requests[1])
	if !containsSubstring(texts, "1 of 3") {
		t.Errorf("second request ambient segments = %v, want the nudge to report attempt 1 of 3", texts)
	}
}

// TestMaxTokensBoundTripsAndRecordsHonestError proves the loop-safety bound:
// a model pathologically re-emitting oversized output must not loop
// forever. With MaxTokensContinuations=3, the 4th consecutive max_tokens
// stop must trip the bound, settle with a classified, named error instead
// of arming a further doomed attempt, and emit exactly one session.error.
func TestMaxTokensBoundTripsAndRecordsHonestError(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopMaxTokens, &message.Text{Text: "chunk1"}),
		asstTurn(provider.StopMaxTokens, &message.Text{Text: "chunk2"}),
		asstTurn(provider.StopMaxTokens, &message.Text{Text: "chunk3"}),
		asstTurn(provider.StopMaxTokens, &message.Text{Text: "chunk4"}),
	}}
	hooks := &fakeHooks{}
	s := NewSession(Config{
		Providers:              provider.Registry{"test": prov},
		Model:                  message.ModelRef{Provider: "test", Model: "m1"},
		MaxTokensContinuations: 3,
		Hooks:                  hooks,
	})

	_, err := s.Prompt(context.Background(), "go")
	if err == nil {
		t.Fatal("Prompt = nil error, want the bound to trip with a named error")
	}
	var exhausted *maxTokensContinuationExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("err = %T (%v), want *maxTokensContinuationExhaustedError", err, err)
	}
	if exhausted.bound != 3 {
		t.Errorf("exhausted.bound = %d, want 3", exhausted.bound)
	}
	if !strings.Contains(err.Error(), "3") || !strings.Contains(err.Error(), "max_tokens") {
		t.Errorf("err.Error() = %q, want it to name the bound (3) and max_tokens", err.Error())
	}

	if len(prov.requests) != 4 {
		t.Fatalf("provider requests = %d, want 4 (initial attempt plus 3 continuations, no 5th doomed attempt)", len(prov.requests))
	}

	h := s.History()
	if len(h) != 5 {
		t.Fatalf("history len = %d, want 5 (user + 4 assistant turns, every one of them kept): %+v", len(h), h)
	}

	var errEvents []plugin.Event
	for _, ev := range hooks.events {
		if ev.Type == plugin.EventSessionError {
			errEvents = append(errEvents, ev)
		}
	}
	if len(errEvents) != 1 {
		t.Fatalf("session.error events = %d, want exactly 1: %+v", len(errEvents), hooks.events)
	}
}

// TestMaxTokensStreakResetsOnSuccess proves the counter is a CONSECUTIVE
// streak, not a lifetime total: with MaxTokensContinuations=1, two
// max_tokens stops separated by a normal tool_use turn must both get a
// continuation -- a cumulative counter would have exhausted the bound on
// the second one.
func TestMaxTokensStreakResetsOnSuccess(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopMaxTokens, &message.Text{Text: "a"}),
		asstTurn(provider.StopToolUse, toolCall("tc1", "bash", `{"command":"echo hi"}`)),
		asstTurn(provider.StopMaxTokens, &message.Text{Text: "b"}),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers:              provider.Registry{"test": prov},
		Model:                  message.ModelRef{Provider: "test", Model: "m1"},
		MaxTokensContinuations: 1,
	})

	final, err := s.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt = %v, want success (the streak must reset after the tool_use turn)", err)
	}
	if final.Parts.Text() != "done" {
		t.Errorf("final = %q, want %q", final.Parts.Text(), "done")
	}
	if len(prov.requests) != 4 {
		t.Fatalf("provider requests = %d, want 4 (both isolated max_tokens stops got their one allowed continuation)", len(prov.requests))
	}
}

// TestMaxTokensContinuationDisabledPreservesOldBehavior proves
// Config.MaxTokensContinuations' zero value keeps the exact pre-fix
// behavior: a max_tokens stop with an orphaned tool call gets its
// synthetic unexecuted-call result and the turn ends immediately, with no
// further model call -- unchanged for a bare embedder engine.Config.
func TestMaxTokensContinuationDisabledPreservesOldBehavior(t *testing.T) {
	tc := toolCall("tc1", "bash", `{"command":"echo hi"}`)
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopMaxTokens, tc),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		// MaxTokensContinuations left at its zero value: disabled.
	})

	asst, err := s.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt = %v, want success", err)
	}
	if asst == nil {
		t.Fatal("Prompt returned nil message")
	}
	if len(prov.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1 (auto-continue disabled: no follow-up call)", len(prov.requests))
	}

	h := s.History()
	if len(h) != 3 || h[2].Role != message.RoleTool {
		t.Fatalf("history = %+v, want [user, assistant, tool(synthetic)]", h)
	}
	if got := s.toolExecutions(); got != 0 {
		t.Errorf("toolExecutions() = %d, want 0", got)
	}
}

// TestTaskChildAutoContinuesMaxTokens confirms the fix applies equally to a
// task child's own turn loop, not only a root session's -- the incident's
// own emphasis. A child Session runs through the identical runAgenticLoop
// (SessionManager.configSnapshot copies the whole parent engine.Config,
// including MaxTokensContinuations, into childCfg -- see Spawn), so this
// asserts against the CHILD's own provider request count and history,
// never the root's.
func TestTaskChildAutoContinuesMaxTokens(t *testing.T) {
	rootProv := &scriptedProvider{name: "root"}
	childProv := &scriptedProvider{name: "child", turns: [][]provider.Event{
		asstTurn(provider.StopMaxTokens, &message.Text{Text: "partial"}),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "child done"}),
	}}
	cfg := managedConfig("root", rootProv, childProv)
	cfg.MaxTokensContinuations = 3

	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(cfg)

	childID, err := mgr.Spawn(SpawnOptions{
		ParentID:  root.ID,
		Prompt:    "go",
		Model:     modelFor("child"),
		AgentType: AgentGeneralPurpose,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	if len(childProv.requests) != 2 {
		t.Fatalf("child provider requests = %d, want 2 (the child's own turn loop must auto-continue too)", len(childProv.requests))
	}
	child, ok := mgr.Session(childID)
	if !ok {
		t.Fatal("child session not found")
	}
	h := child.History()
	if len(h) != 3 {
		t.Fatalf("child history len = %d, want 3 (user, assistant(partial), assistant(child done)): %+v", len(h), h)
	}
	if h[2].Parts.Text() != "child done" {
		t.Errorf("child final text = %q, want %q", h[2].Parts.Text(), "child done")
	}
}
