package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
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

// TestMaxTokensBudgetSpansToolUseRounds proves MaxTokensContinuations is a
// single PER-PROMPT budget that persists across an intervening tool_use
// round, not a counter that resets on one: with a bound of 2, two isolated
// max_tokens stops separated by a normal tool_use turn each consume one
// unit of the SAME budget and both still get their continuation (2 used,
// 2 allowed).
func TestMaxTokensBudgetSpansToolUseRounds(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopMaxTokens, &message.Text{Text: "a"}),
		asstTurn(provider.StopToolUse, toolCall("tc1", "bash", `{"command":"echo hi"}`)),
		asstTurn(provider.StopMaxTokens, &message.Text{Text: "b"}),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers:              provider.Registry{"test": prov},
		Model:                  message.ModelRef{Provider: "test", Model: "m1"},
		MaxTokensContinuations: 2,
	})

	final, err := s.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt = %v, want success (both max_tokens stops fit the shared budget of 2)", err)
	}
	if final.Parts.Text() != "done" {
		t.Errorf("final = %q, want %q", final.Parts.Text(), "done")
	}
	if len(prov.requests) != 4 {
		t.Fatalf("provider requests = %d, want 4 (both isolated max_tokens stops got their continuation)", len(prov.requests))
	}
}

// TestMaxTokensBudgetDoesNotResetOnToolUse is the red-first guard for
// adversarial review finding 3: an earlier version of this counter
// (maxTokensStreak) reset to zero on ANY StopToolUse, including a denied,
// unknown, or failing tool call that never touches toolExecCount -- which
// let a model alternate max_tokens and tool_use indefinitely inside one
// Prompt call, spending an unbounded number of continuations without ever
// tripping Config.MaxTokensContinuations. With a bound of 1, this proves
// the SECOND max_tokens stop -- separated from the first by a genuine
// tool_use round -- does NOT get a fresh continuation: the budget was
// already spent by the first one and must stay spent for the rest of this
// Prompt call.
//
// Red-verify: against the pre-fix runAgenticLoop (maxTokensStreak reset to
// 0 on the StopToolUse branch), this exact sequence succeeds with 4
// requests and no error -- see TestMaxTokensBudgetSpansToolUseRounds above,
// which is that old behavior preserved at a bound wide enough to still
// legitimately allow both stops.
func TestMaxTokensBudgetDoesNotResetOnToolUse(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopMaxTokens, &message.Text{Text: "a"}),
		asstTurn(provider.StopToolUse, toolCall("tc1", "bash", `{"command":"echo hi"}`)),
		asstTurn(provider.StopMaxTokens, &message.Text{Text: "b"}),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	hooks := &fakeHooks{}
	s := NewSession(Config{
		Providers:              provider.Registry{"test": prov},
		Model:                  message.ModelRef{Provider: "test", Model: "m1"},
		MaxTokensContinuations: 1,
		Hooks:                  hooks,
	})

	_, err := s.Prompt(context.Background(), "go")
	if err == nil {
		t.Fatal("Prompt = nil error, want the budget to stay spent across the intervening tool_use round")
	}
	var exhausted *maxTokensContinuationExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("err = %T (%v), want *maxTokensContinuationExhaustedError", err, err)
	}
	if exhausted.bound != 1 {
		t.Errorf("exhausted.bound = %d, want 1", exhausted.bound)
	}
	if !provider.AsPermanent(err) {
		t.Errorf("err = %v, want provider.AsPermanent (finding 5: fail-fast for goal retry)", err)
	}

	// Requests in order: initial (max_tokens "a"), continuation (consumes
	// the bound-of-1 budget, tool_use "tc1"), a THIRD request after the
	// tool ran that hits max_tokens again ("b") and finds the budget
	// already spent -- no 4th request is ever issued.
	if len(prov.requests) != 3 {
		t.Fatalf("provider requests = %d, want 3 (no continuation granted for the second max_tokens stop)", len(prov.requests))
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

// sequencedProvider serves a fixed sequence of outcomes, one per Stream call
// in order -- either a completed turn (events) or an error. It generalizes
// scriptedProvider (every call succeeds) and flakyProvider (a fixed prefix
// of failures, then one fixed turn repeated) for a test that needs an
// ARBITRARY mix of failures and successes across a longer call sequence.
type sequencedProvider struct {
	name     string
	outcomes []sequencedOutcome
	call     int
	requests []*provider.Request
}

type sequencedOutcome struct {
	err    error
	events []provider.Event
}

func (p *sequencedProvider) Name() string { return p.name }

func (p *sequencedProvider) Stream(_ context.Context, req *provider.Request) (provider.Stream, error) {
	p.requests = append(p.requests, req)
	o := p.outcomes[p.call]
	p.call++
	if o.err != nil {
		return nil, o.err
	}
	return &scriptedStream{events: o.events}, nil
}

// TestMaxTokensDropsInvalidPartialToolCall is the red-first guard for
// adversarial review finding 1: a genuinely MID-EMISSION tool call -- one
// the provider cut off with max_tokens before its own content_block_stop
// ever assembled a complete tool_use block -- carries invalid, truncated
// JSON in Arguments (e.g. `{"comm`, never `{"command":"echo hi"}`). Every
// earlier test in this file (TestMaxTokensWithToolCallAutoContinues,
// notably) used a fully VALID tool call and so never exercised this shape.
// The fix: drop that partial call from asst entirely before it is ever
// appended to history, and synthesize no unexecuted-call result for it --
// the model has no usable intent to react to and simply re-issues the call,
// complete, once it continues.
func TestMaxTokensDropsInvalidPartialToolCall(t *testing.T) {
	tc := toolCall("tc1", "bash", `{"comm`)
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
		t.Fatalf("Prompt = %v, want success", err)
	}
	if final.Parts.Text() != "done" {
		t.Errorf("final = %q, want %q", final.Parts.Text(), "done")
	}
	if len(prov.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(prov.requests))
	}

	h := s.History()
	if len(h) != 3 {
		t.Fatalf("history len = %d, want 3 (user, assistant(partial call dropped, no synthetic tool message), assistant(done)): %+v", len(h), h)
	}
	if h[1].Role != message.RoleAssistant {
		t.Fatalf("h[1].Role = %s, want assistant", h[1].Role)
	}
	for _, p := range h[1].Parts {
		if tc, ok := p.(*message.ToolCall); ok {
			t.Fatalf("h[1] still carries a ToolCall part %+v, want the invalid partial call dropped entirely", tc)
		}
	}
	if h[2].Role != message.RoleAssistant || h[2].Parts.Text() != "done" {
		t.Fatalf("h[2] = %+v, want the assistant(done) follow-up", h[2])
	}
	if got := s.toolExecutions(); got != 0 {
		t.Errorf("toolExecutions() = %d, want 0 (a truncated call must never actually run)", got)
	}

	// The follow-up request must replay NO tool_use block at all for the
	// dropped call -- unlike a complete-but-unexecuted call, which stays in
	// history and IS replayed alongside its synthetic is_error result.
	for _, m := range prov.requests[1].Messages {
		for _, p := range m.Parts {
			if tc, ok := p.(*message.ToolCall); ok {
				t.Fatalf("continuation request replays a ToolCall %+v, want the invalid partial dropped", tc)
			}
		}
	}
}

// TestMaxTokensContinuationAppendsGenuineNewUserMessage is the red-first
// guard for adversarial review finding 2: the continuation nudge must
// arrive as a genuine NEW user-role message appended AFTER the truncated
// assistant turn (and its synthetic tool result, if any) -- ending the
// canonical request with RoleUser -- never glued onto an earlier existing
// user message via withAmbientStatus. The old shape left the request
// ending in RoleAssistant/RoleTool: Anthropic serializes that as assistant
// PREFILL, which some models reject outright with a permanent 400, and even
// an accepting model saw a "continue" instruction that chronologically
// precedes the very output it refers to.
//
// Red-verify: against the pre-fix continuationNudgeSegment call site
// (withAmbientStatus(messages, seg), scanning backward for the newest
// EXISTING RoleUser message), the continuation request's trailing message
// is the synthetic tool-role result, not a new RoleUser message -- the
// first assertion below fails.
func TestMaxTokensContinuationAppendsGenuineNewUserMessage(t *testing.T) {
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

	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt = %v, want success", err)
	}

	req := prov.requests[1]
	last := req.Messages[len(req.Messages)-1]
	if last.Role != message.RoleUser {
		t.Fatalf("continuation request's trailing message role = %s, want user (a genuine new turn, not assistant/tool prefill)", last.Role)
	}
	if last.ID == req.Messages[0].ID {
		t.Fatalf("nudge landed on the original user message %q, want a distinct new trailing message", last.ID)
	}
	var nudgeText string
	for _, p := range last.Parts {
		if ec, ok := p.(*message.EngineContext); ok && strings.Contains(ec.Text, "[continuation:") {
			nudgeText = ec.Text
		}
	}
	if nudgeText == "" {
		t.Fatalf("trailing user message parts = %+v, want the continuation nudge as its own EngineContext part", last.Parts)
	}
}

// TestMaxTokensContinuationDrainsQueuedPrompt is the red-first guard for
// adversarial review finding 4: an operator prompt queued while a
// max_tokens turn is in flight must be delivered on the very next
// continuation request -- the same mid-turn steering granularity the
// tool-call-boundary drain already gives a StopToolUse round -- rather than
// waiting undelivered for the whole continuation chain (or the whole Prompt
// call) to finish.
//
// Red-verify: against the pre-fix continuation branch (which loops back to
// streamTurnWithRetry with no drain call at all), the queued prompt is
// still sitting in the queue when the continuation request is built, so the
// "OPERATOR MESSAGES" assertion below fails and QueuedPrompts is non-empty
// after Prompt returns.
func TestMaxTokensContinuationDrainsQueuedPrompt(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopMaxTokens, &message.Text{Text: "partial"}),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers:              provider.Registry{"test": prov},
		Model:                  message.ModelRef{Provider: "test", Model: "m1"},
		MaxTokensContinuations: 3,
	})
	if _, err := s.EnqueuePrompt("steer now"); err != nil {
		t.Fatalf("EnqueuePrompt = %v", err)
	}

	final, err := s.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt = %v, want success", err)
	}
	if final.Parts.Text() != "done" {
		t.Errorf("final = %q, want %q", final.Parts.Text(), "done")
	}
	if pending := s.QueuedPrompts(); len(pending) != 0 {
		t.Fatalf("QueuedPrompts after continuation = %+v, want drained", pending)
	}
	if len(prov.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(prov.requests))
	}

	req := prov.requests[1]
	if len(req.Messages) < 2 {
		t.Fatalf("continuation request has %d messages, want at least 2 (the drained operator message plus the nudge)", len(req.Messages))
	}
	// The durable operator drain (a real appended message) must precede the
	// ephemeral nudge (appended after it, never persisted) -- the operator
	// block is therefore the SECOND-TO-LAST message, the nudge the last.
	operator := req.Messages[len(req.Messages)-2]
	if operator.Role != message.RoleUser {
		t.Fatalf("operator message role = %s, want user", operator.Role)
	}
	text := operator.Parts.Text()
	if !strings.Contains(text, "OPERATOR MESSAGES") || !strings.Contains(text, "steer now") {
		t.Fatalf("operator message = %q, want the labeled operator block with the queued text", text)
	}
	if !strings.Contains(text, "continue the task") {
		t.Errorf("operator message = %q, want plain-turn wording (continue the task)", text)
	}
}

// TestMaxTokensNudgeAbsentOnThirdRequest extends
// TestMaxTokensWithToolCallAutoContinues (which only ever issues two
// requests, so it cannot show the nudge disappearing again) to a THIRD
// request within the same Prompt call -- a genuine tool_use round that
// follows the continuation. Closes adversarial review finding 6's first
// test gap: the nudge must be present on request 2 (the continuation) and
// absent again on request 3, proving pendingContinuationNudge is actually
// cleared once its one streamTurnWithRetry call returns, not merely never
// re-armed.
func TestMaxTokensNudgeAbsentOnThirdRequest(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopMaxTokens, &message.Text{Text: "partial"}),
		asstTurn(provider.StopToolUse, toolCall("tc1", "bash", `{"command":"echo hi"}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
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
	if final.Parts.Text() != "done" {
		t.Errorf("final = %q, want %q", final.Parts.Text(), "done")
	}
	if len(prov.requests) != 3 {
		t.Fatalf("provider requests = %d, want 3 (initial max_tokens, continuation, tool_use follow-up)", len(prov.requests))
	}
	if !containsSubstring(engineContextTexts(prov.requests[1]), "[continuation:") {
		t.Errorf("request 2 (the continuation) ambient segments = %v, want the nudge", engineContextTexts(prov.requests[1]))
	}
	if containsSubstring(engineContextTexts(prov.requests[2]), "[continuation:") {
		t.Errorf("request 3 ambient segments = %v, want no continuation nudge (it must clear after request 2)", engineContextTexts(prov.requests[2]))
	}
}

// TestMaxTokensNudgeNotPersistedAcrossReload closes adversarial review
// finding 6's second test gap: LoadSession -- the production resume path,
// not a hand-built replay -- must never see the continuation nudge in the
// durable log. The nudge is appended only to streamTurn's own throwaway
// per-request message copy (see appendContinuationNudgeMessage), never to
// s.history, so a reloaded session's history must carry no
// *message.EngineContext part naming a continuation anywhere.
func TestMaxTokensNudgeNotPersistedAcrossReload(t *testing.T) {
	dir := t.TempDir()
	tc := toolCall("tc1", "bash", `{"command":"echo hi"}`)
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopMaxTokens, tc),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	cfg := persistCfg(dir, prov)
	cfg.MaxTokensContinuations = 3
	s := NewSession(cfg)

	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt = %v, want success", err)
	}

	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatalf("LoadSession = %v", err)
	}
	for _, m := range loaded.History() {
		for _, p := range m.Parts {
			if ec, ok := p.(*message.EngineContext); ok && strings.Contains(ec.Text, "[continuation:") {
				t.Fatalf("reloaded history carries a persisted continuation nudge on message %s: %q", m.ID, ec.Text)
			}
		}
	}
	if loaded.pendingContinuationNudge != "" {
		t.Errorf("loaded.pendingContinuationNudge = %q, want empty after reload", loaded.pendingContinuationNudge)
	}
}

// TestMaxTokensNudgeSurvivesTransientRetryButNotFutureTurn closes
// adversarial review finding 6's third test gap. It forces a genuine
// transient-error retry INSIDE the continuation's own streamTurnWithRetry
// call (attempt 1 fails with a classified retryable server_error, attempt 2
// succeeds), proving the nudge rides both attempts of that one call, then
// issues a SECOND, wholly unrelated Prompt call on the same session and
// proves the nudge does not resurrect there.
func TestMaxTokensNudgeSurvivesTransientRetryButNotFutureTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prov := &sequencedProvider{name: "test", outcomes: []sequencedOutcome{
			{events: asstTurn(provider.StopMaxTokens, &message.Text{Text: "partial"})},
			{err: retryableServerErr()},
			{events: asstTurn(provider.StopEndTurn, &message.Text{Text: "done"})},
			{events: asstTurn(provider.StopEndTurn, &message.Text{Text: "second done"})},
		}}
		s := NewSession(Config{
			Providers:              provider.Registry{"test": prov},
			Model:                  message.ModelRef{Provider: "test", Model: "m1"},
			MaxTokensContinuations: 3,
			PromptRetries:          1,
		})

		final, err := s.Prompt(context.Background(), "go")
		if err != nil {
			t.Fatalf("Prompt = %v, want success (the transient retry must be masked)", err)
		}
		if final.Parts.Text() != "done" {
			t.Errorf("final = %q, want %q", final.Parts.Text(), "done")
		}
		if len(prov.requests) != 3 {
			t.Fatalf("provider requests = %d, want 3 (initial max_tokens, continuation attempt 1 (fails), continuation attempt 2 (succeeds))", len(prov.requests))
		}
		for _, reqIdx := range []int{1, 2} {
			texts := engineContextTexts(prov.requests[reqIdx])
			if !containsSubstring(texts, "[continuation:") {
				t.Errorf("request %d ambient segments = %v, want the nudge (it must ride every attempt of the same streamTurnWithRetry call)", reqIdx, texts)
			}
		}

		final2, err := s.Prompt(context.Background(), "go again")
		if err != nil {
			t.Fatalf("second Prompt = %v, want success", err)
		}
		if final2.Parts.Text() != "second done" {
			t.Errorf("second final = %q, want %q", final2.Parts.Text(), "second done")
		}
		if len(prov.requests) != 4 {
			t.Fatalf("provider requests = %d, want 4 after the second Prompt call", len(prov.requests))
		}
		if containsSubstring(engineContextTexts(prov.requests[3]), "[continuation:") {
			t.Errorf("second Prompt's request ambient segments = %v, want no continuation nudge (must not leak into a later, unrelated turn)", engineContextTexts(prov.requests[3]))
		}
	})
}

// TestPursueGoalMaxTokensExhaustionFailsFastForGoalRetry is the red-first
// guard for adversarial review finding 5, exercised at the goal-loop layer
// (not just engine.AsPermanent in isolation): with the default-shaped bound
// of 3, one worker attempt that exhausts Config.MaxTokensContinuations
// already makes bound+1 = 4 completed, fully billed max_tokens calls.
// Before maxTokensContinuationExhaustedError was classified
// provider.MarkPermanent, promptTurnWithRetry's deterministic
// goalWorkerRetries budget (2 additional attempts) retried the whole
// exhausted chain from scratch, multiplying 4 calls into
// (goalWorkerRetries+1)*4 = 12 for one goal boundary. This proves exactly 4
// worker calls are made, not 12, and that the goal PARKS (stays resumable)
// rather than clears -- the same shape every other permanent-classified
// worker error already gets (see promptTurnWithRetry's provider.AsPermanent
// branch and TestPursueGoalPermanentWorkerErrorParksAfterOneAttempt in
// goal_permanent_error_test.go, whose shape this mirrors).
func TestPursueGoalMaxTokensExhaustionFailsFastForGoalRetry(t *testing.T) {
	dir := t.TempDir()
	// 12 consecutive max_tokens turns: enough to service every attempt the
	// pre-fix (goalWorkerRetries+1)*4 = 12-call multiplication could burn
	// through, so a regression that reintroduces it runs to completion
	// (and this test's own worker-call assertion catches it) instead of
	// the provider ever running dry mid-test.
	var turns [][]provider.Event
	for i := 0; i < 12; i++ {
		turns = append(turns, asstTurn(provider.StopMaxTokens, &message.Text{Text: "chunk"}))
	}
	prov := &goalProvider{
		worker: turns,
		eval:   [][]provider.Event{evalTurn("MET: done")},
	}
	s := goalSession(t, prov, dir)
	s.cfg.MaxTokensContinuations = 3

	_, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
	if err == nil {
		t.Fatal("PursueGoal = nil error, want the exhausted continuation chain to park")
	}
	if !provider.AsPermanent(err) {
		t.Errorf("err = %v, want provider.AsPermanent", err)
	}
	if !IsGoalWorkerParked(err) {
		t.Fatalf("err = %v, want IsGoalWorkerParked", err)
	}
	if prov.workerCall != 4 {
		t.Fatalf("worker provider calls = %d, want 4 (bound+1 = 3+1, exactly ONE worker attempt -- no goalWorkerRetries multiplication)", prov.workerCall)
	}

	if cond, ok := s.ActiveGoal(); !ok || cond != "cond" {
		t.Fatalf("ActiveGoal = %q, %v; want still active after a permanent-error park", cond, ok)
	}
}
