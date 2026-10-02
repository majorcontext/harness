package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

// TestClaudeCodeAskUserQuestionGate pins the reported problem: without
// --permission-prompt-tool stdio the child never registers AskUserQuestion.
// The flag must stay off unless the embedder opts in, and off for a task
// child or a goal-supervised session, where nothing answers a parked call.
func TestClaudeCodeAskUserQuestionGate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ask    bool
		parent string
		goal   bool
		want   bool
	}{
		{"dormant by default", false, "", false, false},
		{"root session opted in", true, "", false, true},
		{"task child", true, "ses_parent", false, false},
		{"goal armed", true, "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, logPath := claudeCodeTestSession(t, "normal")
			s.cfg.ClaudeCode.AskUserQuestion = tc.ask
			s.cfg.TaskParentID = tc.parent
			if tc.goal {
				if err := s.RegisterGoal("ship it"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Prompt(context.Background(), "hi"); err != nil {
				t.Fatalf("Prompt: %v", err)
			}
			argv := readInvocations(t, logPath)[0]
			if got, _ := argvValueAfter(argv, "--permission-prompt-tool"); (got == "stdio") != tc.want {
				t.Fatalf("--permission-prompt-tool stdio present = %v, want %v: %v", got == "stdio", tc.want, argv)
			}
			settings, _ := argvValueAfter(argv, "--settings")
			disallowed, _ := argvValueAfter(argv, "--disallowedTools")
			if tc.want && (!strings.Contains(settings, `"matcher":"AskUserQuestion"`) || !strings.Contains(settings, "defer") ||
				!strings.HasSuffix(disallowed, ",EnterPlanMode,ExitPlanMode")) {
				t.Errorf("opted-in argv lacks the defer hook or the plan-mode ban: settings=%q disallowed=%q", settings, disallowed)
			}
		})
	}
}

// TestClaudeCodeUnansweredPermissionRequestIsDenied pins the hang the stdio
// permission surface introduces: outside bypassPermissions an ordinary tool
// sends can_use_tool and blocks until the host answers it on stdin.
func TestClaudeCodeUnansweredPermissionRequestIsDenied(t *testing.T) {
	s, _ := claudeCodeTestSession(t, "normal")
	frames := `{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Bash","tool_use_id":"toolu_b","input":{}}}` + "\n" +
		`{"type":"result","subtype":"success","num_turns":1}` + "\n"
	var sent []string
	s.consumeClaudeCodeStream(strings.NewReader(frames), s.Model(), nil, func(b []byte) { sent = append(sent, string(b)) })
	if len(sent) != 1 {
		t.Fatalf("control responses sent = %d, want 1: %q", len(sent), sent)
	}
	got := decodeControlResponse(t, sent[0])
	if got.Response.RequestID != "r1" || got.Response.Response.Behavior != "deny" {
		t.Errorf("control response = %s, want a deny for r1", sent[0])
	}
}

// TestClaudeCodeAnswerResumesParkedQuestion pins the park-and-resume
// contract: a tool_deferred result is a parked turn, not a finished one, and
// the answer turn writes no driving text, only the answer for that call.
func TestClaudeCodeAnswerResumesParkedQuestion(t *testing.T) {
	s, stdinLog := claudeCodeQuestionSession(t)
	if _, err := s.Prompt(context.Background(), "pick a db"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := s.PendingQuestion(); got != "toolu_q" {
		t.Fatalf("PendingQuestion() after the parked turn = %q, want toolu_q", got)
	}
	if _, err := s.AnswerQuestion(context.Background(), "toolu_q", map[string]string{"Which database?": "SQLite"}); err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	lines := readStdinLines(t, stdinLog)
	if len(lines) != 2 {
		t.Fatalf("stdin lines = %d, want the prompt then one control response: %q", len(lines), lines)
	}
	got := decodeControlResponse(t, lines[1])
	if got.Response.Response.Behavior != "allow" || got.Response.Response.UpdatedInput.Answers["Which database?"] != "SQLite" ||
		len(got.Response.Response.UpdatedInput.Questions) != 1 {
		t.Errorf("answer turn control response = %s, want allow with the questions and the answer", lines[1])
	}
	if s.PendingQuestion() != "" {
		t.Errorf("PendingQuestion() after the answer = %q, want empty", s.PendingQuestion())
	}
}

// TestClaudeCodePromptDismissesParkedQuestion pins the lost-reply hazard: a
// resumed CLI re-runs its deferred call before it reads stdin, so a prompt
// written on that turn is answered in a second result harness never reads.
// The parked call must be dismissed first, with no text on that child. The
// fake dismisses the way the real CLI does, with an error result, no init,
// and exit 1, none of which may surface as the turn's error, a metrics
// record, or a history directive on the turn after it.
func TestClaudeCodePromptDismissesParkedQuestion(t *testing.T) {
	s, stdinLog := claudeCodeQuestionSession(t)
	var metrics int
	s.cfg.OnTurnMetrics = func(TurnMetrics) { metrics++ }
	if _, err := s.Prompt(context.Background(), "pick a db"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	msg, err := s.Prompt(context.Background(), "use the default")
	if err != nil {
		t.Fatalf("second Prompt: %v", err)
	}
	lines := readStdinLines(t, stdinLog)
	if len(lines) != 3 || !strings.Contains(lines[2], "use the default") {
		t.Fatalf("stdin lines = %q, want prompt, dismissal, then the new prompt", lines)
	}
	got := decodeControlResponse(t, lines[1])
	if got.Response.Response.Behavior != "deny" || !got.Response.Response.Interrupt {
		t.Errorf("dismissal = %s, want deny with interrupt", lines[1])
	}
	if s.PendingQuestion() != "" || msg.Parts.Text() != "Done — it printed hi." {
		t.Errorf("after dismissal PendingQuestion()=%q reply=%q, want empty and the new turn's reply", s.PendingQuestion(), msg.Parts.Text())
	}
	if metrics != 2 {
		t.Errorf("turn metrics records = %d, want 2: the dismissal made no provider call", metrics)
	}
	assertNoHistoryDirective(t)
}

// TestClaudeCodeParkedQuestionSurvivesReload pins the restart defect:
// LoadSession synthesizes an orphan tool result for a parked question's call
// id. The call is not orphaned, it is waiting, and the answer that arrives
// later must be its only result.
func TestClaudeCodeParkedQuestionSurvivesReload(t *testing.T) {
	s, _ := claudeCodeQuestionSession(t)
	if _, err := s.Prompt(context.Background(), "pick a db"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	r, err := LoadSession(s.cfg, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if got := questionResults(r); got != 0 || r.PendingQuestion() != "toolu_q" {
		t.Fatalf("after reload: %d results for toolu_q, PendingQuestion()=%q; want 0 and toolu_q", got, r.PendingQuestion())
	}
	if ix, err := ReadSessionIndex(s.cfg.SessionDir, s.ID); err != nil || ix.Messages != len(r.History()) {
		t.Errorf("index Messages = %d (err %v), want LoadSession's %d", ix.Messages, err, len(r.History()))
	}
	if _, err := r.AnswerQuestion(context.Background(), "toolu_q", map[string]string{"Which database?": "SQLite"}); err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if got := questionResults(r); got != 1 {
		t.Errorf("results for toolu_q after the answer = %d, want 1", got)
	}
}

// TestClaudeCodeDismissAfterReloadSendsNoHistoryDirective pins the
// watermark after a reload: the dismissal's own result must not read as
// conversation the resumed CLI session has not seen.
func TestClaudeCodeDismissAfterReloadSendsNoHistoryDirective(t *testing.T) {
	s, _ := claudeCodeQuestionSession(t)
	if _, err := s.Prompt(context.Background(), "pick a db"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	r, err := LoadSession(s.cfg, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if _, err := r.Prompt(context.Background(), "use the default"); err != nil {
		t.Fatalf("Prompt after reload: %v", err)
	}
	assertNoHistoryDirective(t)
}

func questionResults(s *Session) int {
	n := 0
	for _, m := range s.History() {
		for _, p := range m.Parts {
			if tr, ok := p.(*message.ToolResult); ok && tr.CallID == "toolu_q" {
				n++
			}
		}
	}
	return n
}

// assertNoHistoryDirective checks the newest child: every turn in these
// tests runs on the same CLI session, so none may ask it to re-read history.
func assertNoHistoryDirective(t *testing.T) {
	t.Helper()
	invocations := readInvocations(t, os.Getenv("FAKE_CLAUDE_LOG"))
	if argv := invocations[len(invocations)-1]; argvContains(argv, claudeCodeHistoryDirective) {
		t.Errorf("last child was sent the get_conversation_history directive: %v", argv)
	}
}

func claudeCodeQuestionSession(t *testing.T) (*Session, string) {
	t.Helper()
	s, _ := claudeCodeTestSession(t, "question")
	s.cfg.ClaudeCode.AskUserQuestion = true
	dir := t.TempDir()
	t.Setenv("FAKE_CLAUDE_STATE", filepath.Join(dir, "parked"))
	stdinLog := filepath.Join(dir, "stdin.log")
	t.Setenv("FAKE_CLAUDE_STDIN_LOG", stdinLog)
	return s, stdinLog
}

func readStdinLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

type controlResponseForTest struct {
	Response struct {
		RequestID string `json:"request_id"`
		Response  struct {
			Behavior     string `json:"behavior"`
			Interrupt    bool   `json:"interrupt"`
			UpdatedInput struct {
				Questions []json.RawMessage `json:"questions"`
				Answers   map[string]string `json:"answers"`
			} `json:"updatedInput"`
		} `json:"response"`
	} `json:"response"`
}

func decodeControlResponse(t *testing.T, line string) controlResponseForTest {
	t.Helper()
	var c controlResponseForTest
	if err := json.Unmarshal([]byte(line), &c); err != nil {
		t.Fatalf("decoding control response %q: %v", line, err)
	}
	return c
}

// TestClaudeCodeQuestionExtraArgsCannotOverrideParkingFlags proves a config
// cannot quietly defeat the defer hook or the plan-mode ban: engine flags
// precede ExtraArgs and the CLI keeps a repeated option's LAST value, so an
// entry here stops a session parking, or restores EnterPlanMode.
func TestClaudeCodeQuestionExtraArgsCannotOverrideParkingFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--settings", "{}"},
		{"--settings={}"},
		{"--permission-prompt-tool", "none"},
		{"--permission-prompt-tool=none"},
		{"--disallowedTools", "Agent"},
		{"--disallowedTools=Agent"},
		{"--disallowed-tools", "Agent"},
		{"--disallowed-tools=Agent"},
		{"--permission-prompts", "none"},
		{"--permission-prompts=none"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			s, _ := claudeCodeQuestionSession(t)
			s.cfg.ClaudeCode.ExtraArgs = args
			_, err := s.Prompt(context.Background(), "hi")
			if err == nil || !strings.Contains(err.Error(), args[0]) {
				t.Fatalf("Prompt error = %v, want a %s conflict", err, args[0])
			}
		})
	}
}

// TestClaudeCodeCompactDismissesParkedQuestion pins the lost-command
// defect: RunCompactCommand reaches the delegated backend directly, so the
// resumed CLI ran its deferred call first and read "/compact" as that call's
// control response. The command must follow a dismissal.
func TestClaudeCodeCompactDismissesParkedQuestion(t *testing.T) {
	s, stdinLog := claudeCodeQuestionSession(t)
	if _, err := s.Prompt(context.Background(), "pick a db"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if _, err := s.RunCompactCommand(context.Background(), CompactOptions{}); err != nil {
		t.Fatalf("RunCompactCommand: %v", err)
	}
	lines := readStdinLines(t, stdinLog)
	if len(lines) != 3 || !strings.Contains(lines[2], compactCommandText) {
		t.Fatalf("stdin lines = %q, want the prompt, the dismissal, then %s", lines, compactCommandText)
	}
	if got := decodeControlResponse(t, lines[1]); got.Response.Response.Behavior != "deny" || !got.Response.Response.Interrupt {
		t.Errorf("dismissal = %s, want deny with interrupt", lines[1])
	}
}

// TestClaudeCodeAnswerSurvivesACrashBetweenItsJournalWrites pins the crash
// window: the clearing claude_code.question record was written BEFORE the
// answer's own tool result, so a process lost between the two reloaded with
// an unanswered call, no exemption, and a synthetic orphan result. The
// result must be durable first, and a reload that lost only the clearing
// record must read the call as answered: the CLI never re-runs a finished
// call, so no later dismissal could clear it.
func TestClaudeCodeAnswerSurvivesACrashBetweenItsJournalWrites(t *testing.T) {
	s, _ := claudeCodeQuestionSession(t)
	if _, err := s.Prompt(context.Background(), "pick a db"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if _, err := s.AnswerQuestion(context.Background(), "toolu_q", map[string]string{"Which database?": "SQLite"}); err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	result, cleared := -1, -1
	var keep []string
	for i, line := range readSessionLog(t, s) {
		switch {
		case strings.Contains(line, `"type":"claude_code.question"`) && !strings.Contains(line, "toolu_q"):
			cleared = i
			continue
		case strings.Contains(line, `"tool_result","call_id":"toolu_q"`):
			result = i
		}
		keep = append(keep, line)
	}
	if result < 0 || cleared < 0 || result > cleared {
		t.Fatalf("journal: toolu_q result at record %d, question cleared at record %d; want the result first", result, cleared)
	}
	rewriteSessionLog(t, s, keep)
	r, err := LoadSession(s.cfg, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if got := r.PendingQuestion(); got != "" {
		t.Errorf("PendingQuestion() for a call that already holds its result = %q, want empty", got)
	}
}

func readSessionLog(t *testing.T, s *Session) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.cfg.SessionDir, s.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

func rewriteSessionLog(t *testing.T, s *Session, lines []string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.cfg.SessionDir, s.ID+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestClaudeCodeDismissalChildDeathSurfaces pins the blanket swallow:
// dismissClaudeCodeQuestion dropped every error the dismissal child returned
// as long as the parked call happened to get a result. A child that died
// before its terminal result then read as a clean dismissal.
func TestClaudeCodeDismissalChildDeathSurfaces(t *testing.T) {
	s, _ := claudeCodeQuestionSession(t)
	if _, err := s.Prompt(context.Background(), "pick a db"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	t.Setenv("FAKE_CLAUDE_DISMISS_DIES", "1")
	if _, err := s.Prompt(context.Background(), "use the default"); err == nil {
		t.Fatal("second Prompt succeeded; want the dismissal child's own failure")
	}
}
