package e2e

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

var updateGoldens = flag.Bool("update", false, "rewrite e2e/testdata/contract goldens")

type scenario struct {
	name       string
	concurrent bool // child sessions race, so requests are ordered by conversation
	chat       bool // the model is a chat-completions gateway, and config names it as provider "bifrost"
	config     map[string]any
	model      []harnesstest.Step
	actions    []action
	driver     func(t *testing.T, modelURL string) driver // nil runs the default HTTP driver
}

type action interface{ run(t *testing.T, r *run) }

type create struct {
	as          string
	staysActive bool // the session never reads idle, so the final waitIdle skips it
}
type submit struct{ as, text string }
type enqueue struct{ as, text string }
type waitIdle struct{ as string }
type interrupt struct{ as string }
type setGoal struct {
	as, condition string
	maxTurns      int
	deferred      bool
}
type release struct{ step string }
type awaitRequests struct{ n int }
type restart struct{ kill bool }
type expectQueued struct {
	as    string
	texts []string
}

type observation struct {
	Requests []normRequest            `json:"requests"`
	Sessions map[string][]normMessage `json:"sessions"`
	Calls    map[string]normCall      `json:"calls,omitempty"`
}

type run struct {
	drv     driver
	fake    *harnesstest.Server
	ids     map[string]string
	aliases []string
	noIdle  map[string]bool
	calls   []recordedCall
	keys    map[string]int
}

// recordedCall is the outcome of one action that reports a result. Its key is
// the action name plus the alias, with a "#n" count suffix on a repeat.
type recordedCall struct {
	key string
	res callResult
}

func (r *run) record(t *testing.T, name, alias string, res callResult) {
	t.Helper()
	key := name
	if alias != "" {
		key += "." + alias
	}
	r.keys[key]++
	if n := r.keys[key]; n > 1 {
		key = fmt.Sprintf("%s#%d", key, n)
	}
	r.calls = append(r.calls, recordedCall{key: key, res: res})
}

func (r *run) id(t *testing.T, alias string) string {
	t.Helper()
	id, ok := r.ids[alias]
	if !ok {
		t.Fatalf("scenario uses alias %q before create", alias)
	}
	return id
}

func (a create) run(t *testing.T, r *run) {
	if _, dup := r.ids[a.as]; dup {
		t.Fatalf("alias %q created twice", a.as)
	}
	r.ids[a.as] = r.drv.Create(t)
	r.aliases = append(r.aliases, a.as)
	r.noIdle[a.as] = a.staysActive
}
func (a submit) run(t *testing.T, r *run)   { r.drv.Submit(t, r.id(t, a.as), a.text) }
func (a enqueue) run(t *testing.T, r *run)  { r.drv.Enqueue(t, r.id(t, a.as), a.text) }
func (a waitIdle) run(t *testing.T, r *run) { r.drv.WaitIdle(t, r.id(t, a.as)) }
func (a interrupt) run(t *testing.T, r *run) {
	r.drv.Interrupt(t, r.id(t, a.as))
}
func (a setGoal) run(t *testing.T, r *run) {
	r.drv.SetGoal(t, r.id(t, a.as), a.condition, a.maxTurns, a.deferred)
}
func (a release) run(_ *testing.T, r *run) { r.fake.Release(a.step) }
func (a awaitRequests) run(t *testing.T, r *run) {
	t.Helper()
	if !r.fake.AwaitRequests(a.n, waitBound) {
		reqs := r.fake.Requests()
		t.Fatalf("waited %s for %d model requests; saw %d: %s\nserve stderr:\n%s", waitBound, a.n, len(reqs), requestSummary(reqs), r.drv.Stderr())
	}
}

func requestSummary(reqs []harnesstest.Request) string {
	var b strings.Builder
	for i, req := range reqs {
		fmt.Fprintf(&b, "\n  %d: last user text %q", i+1, req.LastUserText())
	}
	return b.String()
}
func (a expectQueued) run(t *testing.T, r *run) {
	if got := r.drv.Queued(t, r.id(t, a.as)); !slices.Equal(got, a.texts) {
		t.Fatalf("session %s queue = %q, want %q", a.as, got, a.texts)
	}
}
func (a restart) run(t *testing.T, r *run) { r.drv.Restart(t, a.kill) }

// Actions that record a result under a golden key "calls.<name>.<alias>".
// Each records the response status and body and never fails on a non-2xx
// status, so a scenario can pin an error path.
type compact struct{ as string }
type setModel struct{ as, model string }
type setThinking struct{ as, level string }
type setServiceTier struct{ as, tier string }
type endSession struct{ as string }
type sendToSession struct{ as, text string }
type cancelTree struct{ as string }
type deleteQueued struct{ as string }
type updateGoal struct{ as, condition string }
type clearGoal struct{ as string }
type command struct{ as, text string }

// Observations. A zero beforeSeq, from, or limit is left out of the request.
// listSessions needs at most one resident session: the server lists them in map order.
type listSessions struct{}
type getSession struct{ as string }
type sessionStatus struct{}
type commands struct{}
type messagesPage struct {
	as               string
	beforeSeq, limit int
}
type bootstrap struct {
	as    string
	limit int
}
type journalPage struct {
	as          string
	from, limit int
}

// sseResume reads /event after afterSeq up to the tip seen on entry.
// header sends Last-Event-ID instead of from. as limits the frames to one
// session; scoped asks the server to filter with its session query.
type sseResume struct {
	as             string
	afterSeq       int64
	header, scoped bool
}

// bindChild gives the alias as to the nth child of the session parent, in
// creation order, and waits until that child exists. record adds the alias
// to the final transcript capture; staysActive skips its final idle wait.
type bindChild struct {
	as, parent          string
	nth                 int
	record, staysActive bool
}

func (a compact) run(t *testing.T, r *run) {
	r.record(t, "compact", a.as, r.drv.Compact(t, r.id(t, a.as)))
}
func (a setModel) run(t *testing.T, r *run) {
	r.record(t, "set_model", a.as, r.drv.SetModel(t, r.id(t, a.as), a.model))
}
func (a setThinking) run(t *testing.T, r *run) {
	r.record(t, "set_thinking", a.as, r.drv.SetThinking(t, r.id(t, a.as), a.level))
}
func (a setServiceTier) run(t *testing.T, r *run) {
	r.record(t, "set_service_tier", a.as, r.drv.SetServiceTier(t, r.id(t, a.as), a.tier))
}
func (a endSession) run(t *testing.T, r *run) {
	r.record(t, "end_session", a.as, r.drv.EndSession(t, r.id(t, a.as)))
}
func (a sendToSession) run(t *testing.T, r *run) {
	r.record(t, "send", a.as, r.drv.Send(t, r.id(t, a.as), a.text))
}
func (a cancelTree) run(t *testing.T, r *run) {
	r.record(t, "cancel_tree", a.as, r.drv.CancelTree(t, r.id(t, a.as)))
}
func (a deleteQueued) run(t *testing.T, r *run) {
	r.record(t, "delete_queued", a.as, r.drv.DeleteQueued(t, r.id(t, a.as)))
}
func (a updateGoal) run(t *testing.T, r *run) {
	r.record(t, "update_goal", a.as, r.drv.UpdateGoal(t, r.id(t, a.as), a.condition))
}
func (a clearGoal) run(t *testing.T, r *run) {
	r.record(t, "clear_goal", a.as, r.drv.ClearGoal(t, r.id(t, a.as)))
}
func (a command) run(t *testing.T, r *run) {
	r.record(t, "command", a.as, r.drv.Command(t, r.id(t, a.as), a.text))
}
func (commands) run(t *testing.T, r *run) {
	r.record(t, "commands", "", r.drv.Commands(t))
}
func (listSessions) run(t *testing.T, r *run) {
	r.record(t, "list_sessions", "", r.drv.ListSessions(t))
}
func (a getSession) run(t *testing.T, r *run) {
	r.record(t, "get_session", a.as, r.drv.GetSession(t, r.id(t, a.as)))
}
func (sessionStatus) run(t *testing.T, r *run) {
	r.record(t, "session_status", "", r.drv.SessionStatus(t))
}
func (a messagesPage) run(t *testing.T, r *run) {
	r.record(t, "messages_page", a.as, r.drv.MessagesPage(t, r.id(t, a.as), a.beforeSeq, a.limit))
}
func (a bootstrap) run(t *testing.T, r *run) {
	r.record(t, "bootstrap", a.as, r.drv.Bootstrap(t, r.id(t, a.as), a.limit))
}
func (a journalPage) run(t *testing.T, r *run) {
	r.record(t, "journal_page", a.as, r.drv.JournalPage(t, r.id(t, a.as), a.from, a.limit))
}
func (a sseResume) run(t *testing.T, r *run) {
	id := ""
	if a.as != "" {
		id = r.id(t, a.as)
	}
	r.record(t, "sse_resume", a.as, r.drv.SSEResume(t, id, a.afterSeq, a.header, a.scoped))
}
func (a bindChild) run(t *testing.T, r *run) {
	if _, dup := r.ids[a.as]; dup {
		t.Fatalf("alias %q bound twice", a.as)
	}
	r.ids[a.as] = r.drv.Child(t, r.id(t, a.parent), a.nth)
	if a.record {
		r.aliases = append(r.aliases, a.as)
		r.noIdle[a.as] = a.staysActive
	}
}

func runScenario(t *testing.T, sc scenario) observation {
	t.Helper()
	fake, config := scenarioFake(t, sc)
	var drv driver
	if sc.driver != nil {
		drv = sc.driver(t, fake.URL())
	} else {
		drv = newHTTPDriverWith(t, fake.URL(), config)
	}
	r := &run{
		drv:    drv,
		fake:   fake,
		ids:    map[string]string{},
		noIdle: map[string]bool{},
		keys:   map[string]int{},
	}
	for _, a := range sc.actions {
		a.run(t, r)
	}
	for _, alias := range r.aliases {
		if !r.noIdle[alias] {
			r.drv.WaitIdle(t, r.ids[alias])
		}
	}
	sessions := map[string][]transcriptMessage{}
	for _, alias := range r.aliases {
		msgs := r.drv.Messages(t, r.ids[alias])
		sessions[alias] = msgs
		for _, v := range messageViolations(msgs) {
			t.Errorf("session %s: %s", alias, v)
		}
	}
	for _, v := range journalViolations(r.drv.Events(t)) {
		t.Errorf("journal: %s", v)
	}
	reqs := fake.Requests()
	if sc.concurrent {
		reqs = groupByConversation(reqs)
	}
	return normalizeRun(reqs, sessions, r.calls, r.ids, r.drv.Workdir())
}

func runScenarios(t *testing.T, table []scenario) {
	t.Helper()
	skipShort(t)
	for _, sc := range table {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			compareGolden(t, sc.name, runScenario(t, sc))
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return append(b, '\n')
}

func compareGolden(t *testing.T, name string, obs observation) {
	t.Helper()
	path := filepath.Join("testdata", "contract", name+".golden.json")
	got := mustJSON(t, obs)
	if *updateGoldens {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v (run with -update to create it)", err)
	}
	if string(want) != string(got) {
		t.Errorf("observation differs from %s (-golden +got):\n%s", path, lineDiff(string(want), string(got)))
	}
}

func lineDiff(a, b string) string {
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	lcs := make([][]int, len(x)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []string
	i, j := 0, 0
	for i < len(x) || j < len(y) {
		switch {
		case i < len(x) && j < len(y) && x[i] == y[j]:
			i++
			j++
		case j < len(y) && (i == len(x) || lcs[i][j+1] >= lcs[i+1][j]):
			out = append(out, "+ "+y[j])
			j++
		default:
			out = append(out, "- "+x[i])
			i++
		}
	}
	return strings.Join(out, "\n")
}

func TestScenarioRunsTwiceIdentically(t *testing.T) {
	skipShort(t)
	sc := scenario{
		name:  "one_turn",
		model: []harnesstest.Step{{Name: "reply", Reply: harnesstest.Reply{Text: "hello"}}},
		actions: []action{
			create{as: "a"},
			submit{as: "a", text: "hi"},
		},
	}
	first := runScenario(t, sc)
	second := runScenario(t, sc)
	if string(mustJSON(t, first)) != string(mustJSON(t, second)) {
		t.Fatalf("observations differ between runs:\n%s", lineDiff(string(mustJSON(t, first)), string(mustJSON(t, second))))
	}
}
