package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// host starts what a scenario drives: the serve binary, or harness.Runtime
// in process. open takes a config file, the environment of a lane, and the
// serve flags of a lane.
type host struct {
	open func(t *testing.T, configPath string, env map[string]string, args ...string) laneHost
}

// laneHost is a driver plus the reads that a lane action makes through the
// API of its host.
type laneHost interface {
	driver
	awaitAssistantText(t *testing.T, id, text string)
	answerQuestion(t *testing.T, id, callID string, answers map[string]string) callResult
	journalEvents(t *testing.T, id, prefix string) []any
}

var (
	serveHost = host{func(t *testing.T, configPath string, env map[string]string, args ...string) laneHost {
		return newHTTPDriverAt(t, configPath, env, args...)
	}}
	// runtimeHost sets env in the process, so a row that passes env runs alone.
	runtimeHost = host{func(t *testing.T, configPath string, env map[string]string, args ...string) laneHost {
		if len(args) > 0 {
			t.Fatalf("the runtime takes no serve flags: %q", args)
		}
		for k, v := range env {
			t.Setenv(k, v)
		}
		return newRuntimeDriver(t, configPath)
	}}
)

// newDriver opens h on the served config of a scenario.
func (h host) newDriver(t *testing.T, modelURL string, extra map[string]any) driver {
	t.Helper()
	return h.open(t, writeGoalConfigWith(t, modelURL, scenarioConfig(extra)), nil)
}

// rowKind is how a contract row maps to the runtime.
type rowKind int

const (
	// rowSame: the runtime observation equals the serve golden.
	rowSame rowKind = iota
	// rowRegolden: the runtime observation has its own golden under
	// testdata/runtime, and the cites account for each difference.
	rowRegolden
	// rowDeleted: the spec deletes what the row pins. It does not run.
	rowDeleted
	// rowPending: the row waits for a fix or a later phase. It does not run.
	rowPending
)

// runtimeRow is the disposition of one row. cites are the lines of
// docs/architecture.md and the finding IDs, phases, or "unowned" that the
// disposition rests on.
type runtimeRow struct {
	kind  rowKind
	cites []string
}

func (r runtimeRow) String() string {
	return fmt.Sprintf("%s: %s", [...]string{"same", "re-golden", "deleted by design", "pending"}[r.kind], strings.Join(r.cites, "; "))
}

// runtimeEnv enables the runtime host. CI runs it in a step that does not gate.
const runtimeEnv = "HARNESS_E2E_RUNTIME"

// onRuntime runs each row of table on the runtime host by its disposition.
// A serial row sets the process environment, so it runs alone.
func onRuntime[S any](t *testing.T, table []S, key func(S) (name string, serial bool), run func(*testing.T, S) observation) {
	t.Helper()
	if os.Getenv(runtimeEnv) == "" {
		return
	}
	t.Run("runtime", func(t *testing.T) {
		for _, sc := range table {
			name, serial := key(sc)
			t.Run(name, func(t *testing.T) {
				row, ok := runtimeRows[name]
				if !ok {
					t.Fatalf("row %s has no runtime disposition", name)
				}
				if row.kind == rowDeleted || row.kind == rowPending {
					t.Skip(row)
				}
				if !serial {
					t.Parallel()
				}
				obs := run(t, sc)
				if row.kind == rowSame {
					compareSame(t, name, obs)
					return
				}
				compareGoldenAt(t, filepath.Join("testdata", "runtime", name+".golden.json"), obs, *updateGoldens)
			})
		}
	})
}

// suiteBreaks are the differences that every row shows on the runtime. Each
// waits for a decision under Open questions in docs/architecture.md. A same
// row compares with its serve golden less these.
var suiteBreaks = []func(*normRequest){dropTool("model"), dropTool("session_info"), dropEngineBanner}

func dropTool(name string) func(*normRequest) {
	return func(r *normRequest) {
		if r.Tools = slices.DeleteFunc(r.Tools, func(n string) bool { return n == name }); len(r.Tools) == 0 {
			r.Tools = nil
		}
	}
}

var engineBanner = regexp.MustCompile(`^<harness-engine-context>\n\[engine: harness <version> · session_sync=\w+ · engine started <time>\]\n</harness-engine-context>$`)

// dropEngineBanner drops the banner part that serve adds to the first user
// message, and the message that a chat wire sends it in.
func dropEngineBanner(r *normRequest) {
	for i := range r.Messages {
		r.Messages[i].Parts = slices.DeleteFunc(r.Messages[i].Parts, func(p normReqPart) bool {
			return p.Kind == "text" && engineBanner.MatchString(p.Text)
		})
	}
	r.Messages = slices.DeleteFunc(r.Messages, func(m normReqMessage) bool { return len(m.Parts) == 0 })
}

// compareSame compares obs with the serve golden of row name less the suite breaks.
func compareSame(t *testing.T, name string, obs observation) {
	t.Helper()
	path := filepath.Join("testdata", "contract", name+".golden.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want observation
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	for i := range want.Requests {
		for _, apply := range suiteBreaks {
			apply(&want.Requests[i])
		}
	}
	if w, g := string(mustJSON(t, want)), string(mustJSON(t, obs)); w != g {
		t.Errorf("runtime differs from %s less the suite breaks (-golden +got):\n%s", path, lineDiff(w, g))
	}
}

var pendingCite = regexp.MustCompile(`^(F\d\d|phase \d|unowned)$`)

// citeErrors reports each cite of row that is neither a pending cite nor a
// line of spec, and a cite list that the kind of row does not allow.
func citeErrors(row runtimeRow, spec string) []string {
	var errs []string
	lines, pending := 0, 0
	for _, c := range row.cites {
		switch {
		case pendingCite.MatchString(c):
			pending++
		case strings.Contains(spec, c):
			lines++
		default:
			errs = append(errs, fmt.Sprintf("cites %q, which is not a line of docs/architecture.md or a finding, phase, or unowned", c))
		}
	}
	switch {
	case row.kind == rowSame && len(row.cites) > 0:
		errs = append(errs, "a same row cites nothing")
	case row.kind == rowRegolden && lines == 0:
		errs = append(errs, "a re-golden row cites at least one line of docs/architecture.md")
	case row.kind == rowDeleted && (lines != 1 || pending > 0):
		errs = append(errs, "a deleted row cites one line of docs/architecture.md")
	case row.kind == rowPending && (lines > 0 || pending == 0):
		errs = append(errs, "a pending row cites only findings, phases, or unowned")
	}
	return errs
}

func goldenNames(t *testing.T, dir string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", dir, "*.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range paths {
		names = append(names, strings.TrimSuffix(filepath.Base(p), ".golden.json"))
	}
	return names
}

// TestRuntimeRows checks that each contract golden has one disposition, that
// each re-golden row has a runtime golden, and that each citation holds.
func TestRuntimeRows(t *testing.T) {
	skipShort(t)
	spec, err := os.ReadFile(filepath.Join("..", "docs", "architecture.md"))
	if err != nil {
		t.Fatal(err)
	}
	serve, runtime := goldenNames(t, "contract"), goldenNames(t, "runtime")
	for _, name := range serve {
		if _, ok := runtimeRows[name]; !ok {
			t.Errorf("golden %s has no runtime disposition", name)
		}
	}
	for name, row := range runtimeRows {
		switch {
		case !slices.Contains(serve, name):
			t.Errorf("disposition %s names no golden", name)
		case row.kind == rowRegolden != slices.Contains(runtime, name):
			t.Errorf("row %s is %s, and a runtime golden exists = %t", name, row, slices.Contains(runtime, name))
		}
		for _, e := range citeErrors(row, string(spec)) {
			t.Errorf("row %s: %s", name, e)
		}
	}
	for _, name := range runtime {
		if _, ok := runtimeRows[name]; !ok {
			t.Errorf("runtime golden %s has no disposition", name)
		}
	}
}
