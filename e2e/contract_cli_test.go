package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/testpoll"
)

// cliHost is a working directory, a session dir, and a config that point the
// harness binary at a scripted model.
type cliHost struct {
	t                 *testing.T
	config, dir, work string
	fake              *harnesstest.Server
}

func newCLIHost(t *testing.T, extra map[string]any, steps ...harnesstest.Step) *cliHost {
	t.Helper()
	fake := harnesstest.New(t, steps...)
	return &cliHost{t: t, fake: fake, dir: t.TempDir(), work: t.TempDir(),
		config: writeGoalConfigWith(t, fake.URL(), scenarioConfig(extra))}
}

// run runs the harness binary with args and returns its output and exit code.
func (h *cliHost) run(args ...string) (stdout, stderr string, code int) {
	h.t.Helper()
	return h.start(args...)()
}

// start starts the harness binary with args, and returns the wait for its
// output and exit code.
func (h *cliHost) start(args ...string) (wait func() (stdout, stderr string, code int)) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.t.Context(), waitBound)
	cmd := exec.CommandContext(ctx, harnessBin, args...)
	cmd.Dir = h.work
	cmd.Env = cleanEnv(map[string]string{"HARNESS_CONFIG": h.config, "HARNESS_SESSION_DIR": h.dir, "ANTHROPIC_API_KEY": "e2e-dummy-key"})
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Start(); err != nil {
		cancel()
		h.t.Fatalf("run %v: %v", args, err)
	}
	return func() (string, string, int) {
		defer cancel()
		code := 0
		err := cmd.Wait()
		var exit *exec.ExitError
		switch {
		case errors.As(err, &exit):
			code = exit.ExitCode()
		case err != nil:
			h.t.Fatalf("run %v: %v", args, err)
		}
		return out.String(), errOut.String(), code
	}
}

// sessionID reads the ID that a run prints on stderr.
func sessionID(t *testing.T, stderr string) string {
	t.Helper()
	for _, line := range strings.Split(stderr, "\n") {
		if id, ok := strings.CutPrefix(line, "session: "); ok {
			return id
		}
	}
	t.Fatalf("stderr names no session:\n%s", stderr)
	return ""
}

func TestContractCLIRunSavesAndContinuesTheSession(t *testing.T) {
	skipShort(t)
	h := newCLIHost(t, nil, replyText("hello"))
	out, errOut, code := h.run("run", "-p", "hi")
	if code != 0 || out != "hello\n" {
		t.Fatalf("run = %d %q, want 0 and the reply\n%s", code, out, errOut)
	}
	id := sessionID(t, errOut)

	list, _, _ := h.run("sessions")
	if fields := strings.Split(strings.TrimSpace(list), "\t"); len(fields) != 3 || fields[0] != id || fields[2] != "2" {
		t.Errorf("sessions = %q, want the session with 2 messages", list)
	}
	raw, _, _ := h.run("sessions", "--json")
	var rows []struct {
		ID       string `json:"id"`
		Messages int    `json:"messages"`
	}
	if err := json.Unmarshal([]byte(raw), &rows); err != nil || len(rows) != 1 || rows[0].ID != id || rows[0].Messages != 2 {
		t.Errorf("sessions --json = %q (%v), want one session with 2 messages", raw, err)
	}

	for _, args := range [][]string{{"run", "-c", "-p", "again"}, {"run", "-r", id, "-p", "third"}} {
		if _, errOut, code := h.run(args...); code != 0 || sessionID(t, errOut) != id {
			t.Fatalf("run %v = %d, want the same session\n%s", args, code, errOut)
		}
	}
	if got := len(h.fake.Requests()); got != 3 {
		t.Fatalf("model requests = %d, want 3", got)
	}
	if got := len(h.fake.Requests()[2].Messages); got != 5 {
		t.Errorf("the third request holds %d messages, want the 5 of the history and the prompt", got)
	}
}

func TestContractCLIRunNoSaveWritesNothing(t *testing.T) {
	skipShort(t)
	h := newCLIHost(t, nil, replyText("hello"))
	out, errOut, code := h.run("run", "-no-save", "-p", "hi")
	if code != 0 || out != "hello\n" || strings.Contains(errOut, "session:") {
		t.Fatalf("run -no-save = %d %q\n%s", code, out, errOut)
	}
	if entries, _ := os.ReadDir(h.dir); len(entries) != 0 {
		t.Errorf("session dir holds %d entries, want none", len(entries))
	}
	if _, errOut, code := h.run("run", "-no-save", "-c", "-p", "hi"); code == 0 || !strings.Contains(errOut, "-no-save") {
		t.Errorf("run -no-save -c = %d %q, want a refusal that names -no-save", code, errOut)
	}
}

func TestContractCLIRunJSONPrintsTheEvents(t *testing.T) {
	skipShort(t)
	h := newCLIHost(t, nil, replyText("hello"))
	out, _, code := h.run("run", "-json", "-p", "hi")
	if code != 0 {
		t.Fatalf("run -json exit %d", code)
	}
	kinds := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var ev struct {
			K string `json:"k"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %q is not an event: %v", line, err)
		}
		kinds[ev.K] = true
	}
	for _, k := range []string{"input.admitted", "turn.started", "item.completed", "turn.ended"} {
		if !kinds[k] {
			t.Errorf("no %s event in:\n%s", k, out)
		}
	}
}

func TestContractCLIRunGoalExitCodes(t *testing.T) {
	skipShort(t)
	met := newCLIHost(t, nil, agentStep("work", "done", true), evaluatorStep("judge", "MET: said done", true))
	if _, errOut, code := met.run("run", "-goal", "say done"); code != 0 || !strings.Contains(errOut, "goal achieved in 1 turn(s)") {
		t.Errorf("met goal = %d, want 0 and the verdict\n%s", code, errOut)
	}
	unmet := newCLIHost(t, nil, agentStep("try", "try", true), evaluatorStep("judge", "NOT MET: keep going", true))
	if _, errOut, code := unmet.run("run", "-goal", "say done", "-goal-max-turns", "2"); code != 3 || !strings.Contains(errOut, "goal not achieved after 2 turn(s)") {
		t.Errorf("unmet goal = %d, want 3 and the verdict\n%s", code, errOut)
	}
}

func TestContractCLIRunTypedCommandPrintsNothingOnSuccess(t *testing.T) {
	skipShort(t)
	h := newCLIHost(t, nil, replyText("hello"))
	_, errOut, _ := h.run("run", "-p", "hi")
	id := sessionID(t, errOut)
	out, errOut, code := h.run("run", "-r", id, "-p", "/thinking high")
	if code != 0 || out != "" {
		t.Fatalf("/thinking = %d %q, want 0 and no output\n%s", code, out, errOut)
	}
	if _, errOut, code := h.run("run", "-r", id, "-p", "/compact abc"); code != 1 || !strings.Contains(errOut, "keep_turns must be a number") {
		t.Errorf("a command with a bad argument = %d, want 1 and its text\n%s", code, errOut)
	}
}

func TestContractCLIRunJSONExitsOneWhenACommandFails(t *testing.T) {
	skipShort(t)
	h := newCLIHost(t, nil, replyText("hello"))
	_, errOut, _ := h.run("run", "-p", "hi")
	id := sessionID(t, errOut)
	out, errOut, code := h.run("run", "-json", "-r", id, "-p", "/compact")
	if code != 1 || !strings.Contains(errOut, "/compact did nothing") || !strings.Contains(out, `"status":"failed"`) {
		t.Errorf("run -json with a failed command = %d, want 1, the text, and the failed record\nstdout: %s\nstderr: %s", code, out, errOut)
	}
}

func TestContractCLIRunRefusesALineBeforeItCreatesASession(t *testing.T) {
	skipShort(t)
	rows := []struct {
		name  string
		extra map[string]any
		args  []string
		want  string
	}{
		{"control_command_without_a_session", nil, []string{"-p", "/compact"}, "needs an existing session"},
		{"command_that_run_does_not_perform", nil, []string{"-p", "/status"}, "not available in this mode"},
		{"unknown_command_on_a_model_api", nil, []string{"-p", "/nope"}, `unknown command "nope"`},
		{"goal_without_an_evaluator_model", map[string]any{"goal_evaluator_model": ""}, []string{"-goal", "say done"}, "goal_evaluator_model must be set"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			h := newCLIHost(t, row.extra, replyText("hello"))
			_, errOut, code := h.run(append([]string{"run"}, row.args...)...)
			if code != 1 || !strings.Contains(errOut, row.want) || strings.Contains("\n"+errOut, "\nsession: ") {
				t.Errorf("run %v = %d, want 1 and %q with no session\n%s", row.args, code, row.want, errOut)
			}
			if entries, _ := os.ReadDir(h.dir); len(entries) != 0 {
				t.Errorf("session dir holds %d entries, want none", len(entries))
			}
		})
	}
}

func dirBytes(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		out[path] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestContractCLIRunRefusesAnUnknownLineOfAResumedSessionBeforeItOpens(t *testing.T) {
	skipShort(t)
	h := newCLIHost(t, nil, replyText("hello"))
	_, errOut, _ := h.run("run", "-p", "hi")
	id := sessionID(t, errOut)
	before := dirBytes(t, h.dir)
	for _, args := range [][]string{{"-r", id}, {"-c"}} {
		_, errOut, code := h.run(append([]string{"run"}, append(args, "-p", "/nope")...)...)
		if code != 1 || !strings.Contains(errOut, `unknown command "nope"`) {
			t.Errorf("run %v = %d, want 1 and the unknown command\n%s", args, code, errOut)
		}
		if after := dirBytes(t, h.dir); !maps.Equal(before, after) {
			t.Errorf("run %v changed the session dir, want the log as it was: a refused line opens nothing", args)
		}
	}
}

func TestContractCLIRunFailedTurnExitsOne(t *testing.T) {
	skipShort(t)
	h := newCLIHost(t, nil, harnesstest.Step{Name: "fail", Reply: harnesstest.Reply{HTTPStatus: 400, ErrorMessage: "bad request body"}, Repeat: true})
	if _, errOut, code := h.run("run", "-p", "hi"); code != 1 || !strings.Contains(errOut, "bad request body") {
		t.Errorf("failed turn = %d, want 1 and the error\n%s", code, errOut)
	}
}

func TestContractCLIRunJSONExitsOneWhenATurnFails(t *testing.T) {
	skipShort(t)
	h := newCLIHost(t, nil, harnesstest.Step{Name: "fail", Reply: harnesstest.Reply{HTTPStatus: 400, ErrorMessage: "bad request body"}, Repeat: true})
	out, errOut, code := h.run("run", "-json", "-p", "hi")
	if code != 1 || !strings.Contains(errOut, "bad request body") || !strings.Contains(out, `"k":"turn.ended"`) {
		t.Errorf("run -json with a failed turn = %d, want 1, the error, and the events\nstdout: %s\nstderr: %s", code, out, errOut)
	}
}

func TestContractCLIRunGoalExitsOneWhenATurnFails(t *testing.T) {
	skipShort(t)
	h := newCLIHost(t, nil, harnesstest.Step{Name: "fail", Reply: harnesstest.Reply{HTTPStatus: 400, ErrorMessage: "bad request body"}, Repeat: true})
	out, errOut, code := h.run("run", "-goal", "say done")
	if code != 1 || !strings.Contains(errOut, "bad request body") || strings.Contains(errOut, "goal not achieved") {
		t.Errorf("run -goal with a failed turn = %d, want 1 and the error\nstdout: %s\nstderr: %s", code, out, errOut)
	}
}

func TestContractCLIRunRetryBeforeAnyTextPrintsNoRestartNotice(t *testing.T) {
	skipShort(t)
	h := newCLIHost(t, nil,
		harnesstest.Step{Name: "broken", Reply: harnesstest.Reply{HTTPStatus: 500, ErrorMessage: "upstream broke"}},
		replyText("recovered"))
	out, errOut, code := h.run("run", "-p", "hi")
	if code != 0 || out != "recovered\n" || strings.Contains(errOut, "re-streaming") {
		t.Errorf("run after a retry before any text = %d %q, want 0, the text, and no restart notice\n%s", code, out, errOut)
	}
}

func TestContractCLIRunRetryAfterStreamedTextPrintsTheRestartNotice(t *testing.T) {
	skipShort(t)
	o := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{APIKey: codexAPIKey, Replies: map[string]harnesstest.CodexReply{"dropped": {Drop: true}}},
		harnesstest.Step{Name: "dropped", Reply: codexText("partial")},
		harnesstest.Step{Name: "retry", Reply: codexText("recovered")})
	h := &cliHost{t: t, dir: t.TempDir(), work: t.TempDir(), config: writeGoalConfigWith(t, o.URL(), codexConfig(o.URL(), false, nil))}
	out, errOut, code := h.run("run", "-p", "hi")
	if code != 0 || out != "partial\nrecovered\n" || !strings.Contains(errOut, "[re-streaming after a transient provider error]") {
		t.Errorf("run after a retry that followed streamed text = %d %q, want 0, the text before and after a line break, and the restart notice\n%s", code, out, errOut)
	}
}

func TestContractCLIRunPrintsTheOutputOfATaskChild(t *testing.T) {
	skipShort(t)
	h := newCLIHost(t, nil, delegation("general-purpose", harnesstest.Reply{Text: "child says hello"})...)
	out, errOut, code := h.run("run", "-p", "delegate")
	if code != 0 || !strings.Contains(out, "child says hello") || !strings.Contains(out, "waiting") {
		t.Errorf("run with a task child = %d, want 0 and the text of the parent and of the child on stdout\nstdout: %s\nstderr: %s", code, out, errOut)
	}
}

func TestContractCLIRunWaitsForATaskChildThatRunsAfterItsParentSettled(t *testing.T) {
	skipShort(t)
	h := newCLIHost(t, nil, delegation("general-purpose", childDone)...)
	wait := h.start("run", "-p", "delegate")
	if !h.fake.AwaitRequests(3, waitBound) {
		t.Fatalf("waited %s for the spawn, the child, and the acknowledgement; saw %d requests", waitBound, len(h.fake.Requests()))
	}
	testpoll.Until(t, waitBound, "the parent has not ended its turn", func() bool {
		for _, data := range dirBytes(t, h.dir) {
			if strings.Contains(data, `"k":"turn.ended"`) {
				return true
			}
		}
		return false
	})
	h.fake.Release("child")
	out, errOut, code := wait()
	if code != 0 || !strings.Contains(out, "waiting") || !strings.Contains(out, "child done") || !strings.Contains(out, "ok") {
		t.Errorf("run with a child that outlives its parent turn = %d, want 0, the child text, and the turn of its report\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if got := len(h.fake.Requests()); got != 4 {
		t.Errorf("model requests = %d, want 4: the report of the child reached the parent", got)
	}
}

// childScript scripts a parent that spawns one child, holds its turn open
// while the child runs, then sends the child more work when the prompt names
// it. The child replies with the text of its task.
func childScript(sendOn harnesstest.Matcher) []harnesstest.Step {
	return []harnesstest.Step{
		busyTaskStep("spawn", userStarts("delegate"), fixed(spawn("general-purpose", "child work"))),
		{Name: "child", Match: userStarts("child work"), Reply: harnesstest.Reply{Text: "first child text"}},
		busyTaskStep("send", sendOn, onKid(func(kid string) []map[string]any {
			return []map[string]any{onSession("send", kid, "prompt", "second work")}
		})),
		{Name: "child again", Match: userStarts("second work"), Reply: harnesstest.Reply{Text: "second child text"}},
		{Name: "rest", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true},
	}
}

func TestContractCLIRunPrintsEachOutputOfATaskChildOnceWhenTheParentSendsToItAfterItSettled(t *testing.T) {
	skipShort(t)
	h := newCLIHost(t, nil, childScript(matchAll(rootStarts("delegate"), harnesstest.LastToolResult("bash")))...)
	out, errOut, code := h.run("run", "-p", "delegate")
	if first, second := strings.Count(out, "first child text"), strings.Count(out, "second child text"); code != 0 || first != 1 || second != 1 {
		t.Errorf("run with a send to a settled child = %d, want 0 and each child text once (first %d, second %d)\nstdout: %s\nstderr: %s", code, first, second, out, errOut)
	}
}

func TestContractCLIRunResumedPrintsOnlyTheNewOutputOfATaskChild(t *testing.T) {
	skipShort(t)
	h := newCLIHost(t, nil, childScript(userStarts("again"))...)
	out, errOut, code := h.run("run", "-p", "delegate")
	if code != 0 || strings.Count(out, "first child text") != 1 {
		t.Fatalf("first run = %d, want 0 and the child text once\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	out, errOut, code = h.run("run", "-r", sessionID(t, errOut), "-p", "again")
	if first, second := strings.Count(out, "first child text"), strings.Count(out, "second child text"); code != 0 || first != 0 || second != 1 {
		t.Errorf("resumed run = %d, want 0 and only the new text of the child (first %d, second %d)\nstdout: %s\nstderr: %s", code, first, second, out, errOut)
	}
}

func TestContractCLIRunJSONPrintsTheEventsOfATaskChildWithTheirSession(t *testing.T) {
	skipShort(t)
	h := newCLIHost(t, nil, delegation("general-purpose", harnesstest.Reply{Text: "child says hello"})...)
	out, errOut, code := h.run("run", "-json", "-p", "delegate")
	if code != 0 {
		t.Fatalf("run -json with a task child = %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	sessions, childText := map[string]bool{}, ""
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var ev struct {
			SessionID string `json:"session_id"`
			K         string `json:"k"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.SessionID == "" {
			t.Fatalf("line %q is not an event with a session_id: %v", line, err)
		}
		sessions[ev.SessionID] = true
		if ev.K == "item.completed" && strings.Contains(line, "child says hello") {
			childText = ev.SessionID
		}
	}
	if len(sessions) != 2 || childText == "" {
		t.Errorf("run -json printed the events of %d sessions, want the parent and the child, with the child text in an item of the child\n%s", len(sessions), out)
	}
}

func TestContractCLISessionsOfAnEmptyDir(t *testing.T) {
	skipShort(t)
	h := newCLIHost(t, nil)
	if out, _, code := h.run("sessions", "--json"); code != 0 || strings.TrimSpace(out) != "[]" {
		t.Errorf("sessions --json of an empty dir = %d %q, want []", code, out)
	}
}

func TestContractCLIPluginProbePrintsHooks(t *testing.T) {
	skipShort(t)
	none := newCLIHost(t, nil)
	if out, _, code := none.run("plugin", "probe"); code != 0 || strings.TrimSpace(out) != "no plugins configured" {
		t.Errorf("plugin probe with none = %d %q", code, out)
	}
	h := newCLIHost(t, pluginConfig(t, nil))
	out, errOut, code := h.run("plugin", "probe")
	if code != 0 || !strings.HasPrefix(out, "fixture: ") || !strings.Contains(out, "system.transform") {
		t.Errorf("plugin probe = %d %q, want the name and the hooks of the fixture\n%s", code, out, errOut)
	}
}

func TestContractCLIRunMaxTokensCapsEachModelResponse(t *testing.T) {
	skipShort(t)
	h := newCLIHost(t, nil, replyText("hello"))
	out, errOut, code := h.run("run", "-max-tokens", "123", "-p", "hi")
	if code != 0 || out != "hello\n" {
		t.Fatalf("run = %d %q, want 0 and the reply\n%s", code, out, errOut)
	}
	if reqs := h.fake.Requests(); len(reqs) == 0 || reqs[0].MaxTokens != 123 {
		t.Errorf("requests = %+v, want a first request with max_tokens 123", reqs)
	}
}

func TestContractCLIRunNegativeMaxTokensReadsAsTheDefault(t *testing.T) {
	skipShort(t)
	h := newCLIHost(t, nil, replyText("hello"))
	out, errOut, code := h.run("run", "-max-tokens", "-1", "-p", "hi")
	if code != 0 || out != "hello\n" {
		t.Fatalf("run = %d %q, want 0 and the reply\n%s", code, out, errOut)
	}
	if reqs := h.fake.Requests(); len(reqs) == 0 || reqs[0].MaxTokens != 8192 {
		t.Errorf("requests = %+v, want a first request with the default max_tokens 8192", reqs)
	}
}
