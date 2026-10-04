package harness_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/protocol"
)

func typed(id, s string) protocol.Input {
	in := text(id, s)
	in.Source = protocol.SourceTyped
	return in
}

// commandDir holds the prompt commands review and bad, and a file whose
// name is not a command name.
func commandDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"review.md":   "---\ndescription: Review a ref\nargument-hint: <ref>\n---\nReview $1 now.\n",
		"bad.md":      "no frontmatter\n",
		"Bad Name.md": "---\ndescription: x\n---\nx\n",
	} {
		path := filepath.Join(dir, ".agents", "commands", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// commandLine renders a command.recorded record, or line for any other kind.
func commandLine(kind string, d json.RawMessage) string {
	if kind != "command.recorded" {
		return line(kind, d)
	}
	var c struct{ InputID, Name, Status, Text string }
	_ = json.Unmarshal(d, &c)
	return strings.TrimSpace(strings.Join([]string{kind, c.Name, c.Status, c.Text}, " "))
}

type commandRow struct {
	name, line string
	source     string
	busy       bool
	receipt    string
	log        []string
	prompt     string
}

var commandRows = []commandRow{
	{name: "a control command runs and records its outcome", line: "/thinking high", receipt: "accepted",
		log: []string{"command.recorded thinking accepted", "settings.changed", "command.recorded thinking succeeded /thinking succeeded"}},
	{name: "bad arguments fail at once", line: "/compact abc", receipt: "failed",
		log: []string{`command.recorded compact failed command: /compact keep_turns must be a number, got "abc"`}},
	{name: "a frontend command is unsupported by its typed name", line: "/clear", receipt: "unsupported",
		log: []string{"command.recorded new unsupported /clear is not available in this client"}},
	{name: "a compaction with nothing to fold fails", line: "/compact", receipt: "accepted",
		log: []string{"command.recorded compact accepted", "command.recorded compact failed /compact did nothing: the session does not have enough turns yet to fold"}},
	{name: "a goal without an evaluator fails", line: "/goal ship it", receipt: "accepted",
		log: []string{"command.recorded goal accepted", "command.recorded goal failed harness: invalid request: a goal needs goal_evaluator_model"}},
	{name: "a command that waits for idle is refused during a turn", line: "/compact", busy: true, receipt: "refused",
		log: []string{"command.recorded compact refused /compact cannot run while a turn is running; send it again after the turn ends"}},
	{name: "a command available during a turn runs", line: "/abort", busy: true, receipt: "accepted",
		log: []string{"command.recorded abort accepted", "item.completed tool c1 " + interrupted, "turn.ended interrupted stopped", "command.recorded abort succeeded /abort succeeded"}},
	{name: "an unknown name is text", line: "/cost", log: []string{"input.admitted a", "turn.started a"}, prompt: "/cost"},
	{name: "a double slash is a literal slash", line: "//compact", log: []string{"input.admitted a", "turn.started a"}, prompt: "/compact"},
	{name: "a prompt command expands", line: "/review HEAD", log: []string{"input.admitted a", "turn.started a"}, prompt: "Review HEAD now.\n"},
	{name: "an untyped line is text", line: "/compact", source: "user", log: []string{"input.admitted a", "turn.started a"}, prompt: "/compact"},
}

func TestTypedCommands(t *testing.T) {
	for _, row := range commandRows {
		t.Run(row.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) { typedCommand(t, row) })
		})
	}
}

func typedCommand(t *testing.T, row commandRow) {
	t.Helper()
	st, f := harness.NewMemStore(), newFake()
	r, err := harness.NewWithBackend(harness.Options{Store: st, WorkDir: commandDir(t)}, f)
	if err != nil {
		t.Fatal(err)
	}
	s := create(t, r)
	after := uint64(2)
	var run fakeRun
	if row.busy {
		submit(t, s, text("busy", "work"))
		run = <-f.runs
		run.emit(callTool("c1"))
		after = 5
	}
	in := typed("a", row.line)
	if row.source != "" {
		in.Source = row.source
	}
	got := submit(t, s, in)
	if got.Command != row.receipt {
		t.Errorf("receipt = %+v, want command %q", got, row.receipt)
	}
	select {
	case next := <-f.runs:
		if text := next.req.Input[0].Parts[0].Text; text != row.prompt {
			t.Errorf("turn input = %q, want %q", text, row.prompt)
		}
		next.end()
	default:
		if row.busy && row.receipt == "refused" {
			run.end()
		}
	}
	var lines []string
	recs, _ := st.Read(bg, "s1", after, 100)
	for _, rec := range recs {
		var env struct {
			K string          `json:"k"`
			D json.RawMessage `json:"d"`
		}
		_ = json.Unmarshal(rec.Data, &env)
		lines = append(lines, commandLine(env.K, env.D))
	}
	if want := row.log; strings.Join(lines[:min(len(want), len(lines))], "\n") != strings.Join(want, "\n") {
		t.Errorf("log =\n%s\nwant it to start\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	closeRuntime(t, r)
}

func TestTypedCommandRepeats(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, f := harness.NewMemStore(), newFake()
		r := runtime(t, st, f)
		s := create(t, r)
		first := submit(t, s, typed("a", "/thinking high"))
		again, repeat, err := s.Admit(bg, typed("a", "/thinking high"))
		if err != nil || !repeat || again.Seq != first.Seq || again.Command != protocol.CommandSucceeded {
			t.Errorf("repeat = %+v %v %v, want seq %d, a repeat, and the terminal status", again, repeat, err, first.Seq)
		}
		for _, in := range []protocol.Input{typed("a", "/thinking low"), text("a", "hi")} {
			if _, err := s.Submit(bg, in); !errors.Is(err, harness.ErrInputConflict) {
				t.Errorf("Submit(%q) = %v, want ErrInputConflict", in.Parts[0].Text, err)
			}
		}
		closeRuntime(t, r)
	})
}

func TestOpenEndsAnUnfinishedCommand(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := harness.NewMemStore()
		r := runtime(t, st, newFake())
		create(t, r)
		closeRuntime(t, r)
		head, _ := st.Head(bg, "s1")
		data, err := eventlog.Envelope{Seq: head + 1, Time: time.Now(), Event: eventlog.CommandRecorded{
			InputID: "a", Line: "/compact", Name: "compact", Status: protocol.CommandAccepted}}.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Append(bg, "s1", head, data); err != nil {
			t.Fatal(err)
		}
		r = runtime(t, st, newFake())
		if _, err := r.Open(bg, "s1"); err != nil {
			t.Fatal(err)
		}
		recs, _ := st.Read(bg, "s1", head+2, 10)
		var got []string
		for _, rec := range recs {
			got = append(got, string(rec.Data))
		}
		if len(got) != 1 || !strings.Contains(got[0], `"status":"interrupted","text":"harness restarted before /compact finished; it will not run again"`) {
			t.Errorf("records after the fence = %q, want one interrupted command", got)
		}
		closeRuntime(t, r)
	})
}

func TestCommandList(t *testing.T) {
	r, err := harness.NewWithBackend(harness.Options{Store: harness.NewMemStore(), WorkDir: commandDir(t)}, newFake())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/commands")
	if err != nil {
		t.Fatal(err)
	}
	var got protocol.Commands
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /commands = %d %v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()
	var names []string
	byName := map[string]protocol.CommandEntry{}
	for _, c := range got.Commands {
		names = append(names, c.Name)
		byName[c.Name] = c
	}
	want := "abort bad compact goal goal-clear model new processes queue queue-clear quit resume review status thinking tier"
	if strings.Join(names, " ") != want {
		t.Errorf("names = %q, want %q", names, want)
	}
	if c := byName["compact"]; c.Method != "POST" || c.Path != "/sessions/{id}/compact" || c.AvailableDuringTask == nil || *c.AvailableDuringTask || c.ArgHint != "" || c.Args != nil {
		t.Errorf("compact = %+v, want POST /sessions/{id}/compact with no args, not during a task", c)
	}
	for _, c := range got.Commands {
		if c.Path == "" {
			continue
		}
		req := httptest.NewRequest(c.Method, strings.ReplaceAll(c.Path, "{id}", "nope"), nil)
		rec := httptest.NewRecorder()
		r.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusMethodNotAllowed || rec.Code == http.StatusNotFound && strings.Contains(rec.Body.String(), protocol.CodeInvalidRequest) {
			t.Errorf("%s %s has no route: %d %s", c.Method, c.Path, rec.Code, rec.Body)
		}
	}
	if c := byName["review"]; c.Kind != "prompt" || c.Summary != "Review a ref" || c.ArgHint != "<ref>" {
		t.Errorf("review = %+v, want the prompt command", c)
	}
	support := map[string]protocol.CommandSupport{
		"abort": {Supported: true}, "compact": {Supported: true}, "review": {Supported: true}, "status": {Supported: true},
		"new": {Reason: "Not available in this client."}, "queue-clear": {Reason: "Not available in this client."},
	}
	for name, s := range support {
		if got.ServeSupport[name] != s {
			t.Errorf("serve_support[%s] = %+v, want %+v", name, got.ServeSupport[name], s)
		}
	}
	if got.ServeSupport["bad"].Supported || got.ServeSupport["bad"].Reason == "" || len(got.DiscoveryErrors) != 1 {
		t.Errorf("bad = %+v, discovery errors %q: want bad disabled with its error and one name error", got.ServeSupport["bad"], got.DiscoveryErrors)
	}
	closeRuntime(t, r)
}

func TestCloseRefusesATypedCommand(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, f := &held{Store: harness.NewMemStore(), hold: make(chan struct{})}, newFake()
		r := runtime(t, st, f)
		s := create(t, r)
		submit(t, s, text("busy", "work"))
		<-f.runs
		st.armed.Store(true)
		closed := make(chan error, 1)
		go func() { closed <- r.Close(bg) }()
		synctest.Wait()
		admitted := make(chan error, 1)
		go func() { _, err := s.Submit(bg, typed("a", "/thinking high")); admitted <- err }()
		synctest.Wait()
		select {
		case err := <-admitted:
			if !errors.Is(err, harness.ErrDraining) {
				t.Errorf("Submit = %v, want ErrDraining", err)
			}
		default:
			t.Error("Submit waits for the session while Close runs, want ErrDraining")
		}
		close(st.hold)
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
	})
}
