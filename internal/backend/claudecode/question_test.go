package claudecode_test

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/protocol"
)

// askRuntime is a Claude Code runtime whose embedder answers questions.
func askRuntime(t *testing.T, st harness.Store, ask bool) *harness.Runtime {
	t.Helper()
	bin, err := fakeClaudeBin()
	if err != nil {
		t.Fatal(err)
	}
	r, err := harness.New(harness.Options{Store: st, AskUserQuestion: ask, Config: config.Config{
		Providers: map[string]config.Provider{"claude-code": {Type: config.TypeClaudeCodeCLI, BinaryPath: bin}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeRuntime(t, r) })
	return r
}

func parkedQuestion(t *testing.T) (*harness.Session, harness.Store, string) {
	t.Helper()
	argvLog := fakeClaude(t, "question", "FAKE_CLAUDE_STATE", filepath.Join(t.TempDir(), "parked"))
	st := harness.NewMemStore()
	s, err := askRuntime(t, st, true).Create(bg, protocol.CreateSession{ID: "s1", Model: "claude-code/sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	turnOf(t, s, text("a", "pick a db"))
	return s, st, argvLog
}

func argValue(argv []string, flag string) string {
	if i := slices.Index(argv, flag); i >= 0 && i+1 < len(argv) {
		return argv[i+1]
	}
	return ""
}

func TestClaudeCodeParksAQuestionAsARequest(t *testing.T) {
	s, st, argvLog := parkedQuestion(t)
	wantLog(t, st, 2, "input.admitted a", "turn.started a", "backend.state", "item.completed assistant toolu_q", "request.opened",
		"backend.state", "turn.ended awaiting_input")
	if v := s.View(); v.Status != protocol.StatusWaiting {
		t.Errorf("status = %s, want waiting", v.Status)
	}
	argv := jsonLines[[]string](t, argvLog)[0]
	if argValue(argv, "--permission-prompt-tool") != "stdio" || !strings.Contains(argValue(argv, "--settings"), "defer") ||
		!strings.Contains(argValue(argv, "--disallowedTools"), "EnterPlanMode,ExitPlanMode") {
		t.Errorf("argv = %q, want the question channel, the defer hook, and no plan mode", argv)
	}
}

func TestClaudeCodeAnswerResumesTheParkedCall(t *testing.T) {
	s, st, argvLog := parkedQuestion(t)
	after := s.View().HeadSeq
	answer := json.RawMessage(`{"Which database?":"SQLite"}`)
	if err := s.Resolve(bg, "toolu_q", protocol.Resolution{Answer: answer}); err != nil {
		t.Fatal(err)
	}
	await(t, s, after, "turn.ended")
	wantLog(t, st, after, "request.resolved", "turn.started",
		`item.completed tool toolu_q {"response":{"request_id":"req-1","response":{"behavior":"allow","updatedInput":{"answers":{"Which database?":"SQLite"},"questions":[{"header":"DB","multiSelect":false,"options":[{"description":"a","label":"PostgreSQL"},{"description":"b","label":"SQLite"}],"question":"Which database?"}]}},"subtype":"success"},"type":"control_response"}`,
		"item.completed assistant Noted.", "backend.state", "turn.ended completed")
	argv := jsonLines[[]string](t, argvLog)
	if len(argv) != 2 || !hasArgs(argv[1], "--resume", "fake-session-1") || !strings.Contains(argValue(argv[1], "--settings"), "toolu_q") {
		t.Fatalf("argv = %q, want a resumed run whose hook passes toolu_q", argv)
	}
	stdin := jsonLines[struct {
		Type     string
		Response struct {
			Subtype  string
			Response struct {
				Behavior     string
				UpdatedInput struct{ Answers map[string]string }
			}
		}
	}](t, strings.TrimSuffix(argvLog, "argv")+"stdin")
	if len(stdin) != 2 || stdin[1].Type != "control_response" || stdin[1].Response.Subtype != "success" ||
		stdin[1].Response.Response.Behavior != "allow" || stdin[1].Response.Response.UpdatedInput.Answers["Which database?"] != "SQLite" {
		t.Errorf("stdin = %+v, want the prompt, then an allow with the answers", stdin)
	}
}

func TestClaudeCodeDismissesAParkedQuestionBeforeTheNextPrompt(t *testing.T) {
	s, st, argvLog := parkedQuestion(t)
	after := s.View().HeadSeq
	turnOf(t, s, text("b", "never mind"))
	wantLog(t, st, after, "request.resolved", "input.admitted b", "turn.started b", "backend.state", "item.completed assistant Let me check that.",
		"item.completed assistant toolu_1", "item.completed tool toolu_1 hi", "item.completed assistant Done — it printed hi.", "context.measured", "turn.ended completed")
	if argv := jsonLines[[]string](t, argvLog); len(argv) != 3 {
		t.Fatalf("CLI runs = %d, want the turn, the dismissal, and the next turn", len(argv))
	}
	stdin := jsonLines[struct {
		Type     string
		Message  struct{ Content string }
		Response struct{ Response struct{ Interrupt bool } }
	}](t, strings.TrimSuffix(argvLog, "argv")+"stdin")
	if len(stdin) != 3 || stdin[1].Type != "control_response" || !stdin[1].Response.Response.Interrupt || stdin[2].Message.Content != "never mind" {
		t.Errorf("stdin = %+v, want the prompt, a denial that interrupts, and the next prompt", stdin)
	}
}

func TestClaudeCodeAsksNoQuestionUnlessTheEmbedderAnswers(t *testing.T) {
	argvLog := fakeClaude(t, "thinking")
	s, err := askRuntime(t, harness.NewMemStore(), false).Create(bg, protocol.CreateSession{ID: "s1", Model: "claude-code/sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	turnOf(t, s, text("a", "hi"))
	if argv := jsonLines[[]string](t, argvLog)[0]; slices.Contains(argv, "--permission-prompt-tool") || slices.Contains(argv, "--settings") {
		t.Errorf("argv = %q, want no question channel", argv)
	}
}
