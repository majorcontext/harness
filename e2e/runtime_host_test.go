package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// host starts what a scenario drives: the serve binary, or harness.Runtime
// in process. openIn takes a config file, the work dir, the environment of a
// lane, and the serve flags of a lane.
type host struct {
	runtime bool
	openIn  func(t *testing.T, configPath, workDir string, env map[string]string, args ...string) laneHost
}

// name is the name of the subtest that runs a row on h.
func (h host) name() string {
	if h.runtime {
		return "runtime"
	}
	return "serve"
}

// open opens h in a new work dir.
func (h host) open(t *testing.T, configPath string, env map[string]string, args ...string) laneHost {
	t.Helper()
	return h.openIn(t, configPath, t.TempDir(), env, args...)
}

// laneHost is a driver plus the reads that a lane action makes through the
// API of its host.
type laneHost interface {
	driver
	awaitAssistantText(t *testing.T, id, text string)
	awaitAdmitted(t *testing.T, id, text string, n int)
	resolveQuestion(t *testing.T, id, callID string, res resolution) callResult
	journalEvents(t *testing.T, id, prefix string) []any
	messageParents(t *testing.T, id string) callResult
	backendStateKeys(t *testing.T, id string) callResult
	compactKeeping(t *testing.T, id string, keep int) callResult
}

var (
	serveHost = host{openIn: func(t *testing.T, configPath, workDir string, env map[string]string, args ...string) laneHost {
		return newServeDriverIn(t, configPath, env, workDir, args...)
	}}
	// runtimeHost sets env in the process, so a row that passes env runs alone.
	runtimeHost = host{runtime: true, openIn: func(t *testing.T, configPath, workDir string, env map[string]string, args ...string) laneHost {
		ask := slices.Contains(args, "--ask-user-question")
		if len(args) > 1 || len(args) == 1 && !ask {
			t.Fatalf("the runtime takes only the serve flag --ask-user-question: %q", args)
		}
		for k, v := range env {
			t.Setenv(k, v)
		}
		return newRuntimeDriverIn(t, configPath, ask, workDir)
	}}
)

// newDriver opens h on the served config of a scenario.
func (h host) newDriver(t *testing.T, modelURL string, extra map[string]any) driver {
	t.Helper()
	return h.open(t, writeGoalConfigWith(t, modelURL, scenarioConfig(extra)), nil)
}

// rowKind is how a contract row maps to a host.
type rowKind int

const (
	// rowSame: the observation equals the engine golden under testdata/contract.
	rowSame rowKind = iota
	// rowRegolden: the runtime observation has its own golden under
	// testdata/runtime, and the cites account for each difference.
	rowRegolden
	// rowDeleted: the spec deletes what the row pins. It does not run.
	rowDeleted
	// rowPending: the row waits for a fix or a later phase. It runs as an
	// expected failure.
	rowPending
)

// runtimeRow is the disposition of one row. cites are the lines of
// docs/architecture.md and the finding IDs and phases that the
// disposition rests on.
type runtimeRow struct {
	kind  rowKind
	cites []string
}

func (r runtimeRow) String() string {
	return fmt.Sprintf("%s: %s", [...]string{"same", "re-golden", "deleted by design", "pending"}[r.kind], strings.Join(r.cites, "; "))
}

// runtimeEnv enables the runtime host, which runs beside the serve host.
const runtimeEnv = "HARNESS_E2E_RUNTIME"

// onHost runs each row of table on h by its disposition. The runtime host
// runs only with runtimeEnv, and not under -update, which the serve host
// writes. A serial row sets the process environment, so it runs alone.
func onHost[S any](t *testing.T, h host, table []S, key func(S) (name string, serial bool), run func(*testing.T, S) observation) {
	t.Helper()
	if h.runtime && (os.Getenv(runtimeEnv) == "" || *updateGoldens) {
		return
	}
	t.Run(h.name(), func(t *testing.T) {
		for _, sc := range table {
			name, serial := key(sc)
			t.Run(name, func(t *testing.T) {
				row, ok := runtimeRows[name]
				if !ok {
					t.Fatalf("row %s has no runtime disposition", name)
				}
				if row.kind == rowDeleted {
					t.Skip(row)
				}
				pending := row.kind == rowPending
				if pending && os.Getenv(pendingBinEnv) == "" {
					expectPendingFailure(t, row)
					return
				}
				if (!serial || !h.runtime) && !pending {
					t.Parallel()
				}
				obs := run(t, sc)
				if row.kind == rowSame || pending {
					compareSame(t, name, obs)
					return
				}
				compareGoldenAt(t, filepath.Join("testdata", "runtime", name+".golden.json"), obs, *updateGoldens && !h.runtime)
			})
		}
	})
}

const (
	// pendingBinEnv holds the binaries of the parent when it runs a pending row
	// in a child test process, which compares the row like a same row.
	pendingBinEnv = "HARNESS_E2E_PENDING_BIN"
	pendingBound  = 5 * time.Minute
)

// pendingWaitBound is the wait bound of a child process. A pending row that
// does not match fails fast or waits for what never comes, so its child waits
// less.
const pendingWaitBound = 15 * time.Second

// pendingSlots bounds the child processes that pending rows start.
var pendingSlots = make(chan struct{}, 4)

// expectPendingFailure runs a pending row in a child test process, so its
// failure does not fail this run, and fails when the row passes: a fix or a
// phase has made it match the engine golden, and it must become a same row.
func expectPendingFailure(t *testing.T, row runtimeRow) {
	t.Helper()
	t.Parallel()
	pendingSlots <- struct{}{}
	defer func() { <-pendingSlots }()
	var parts []string
	for _, p := range strings.Split(t.Name(), "/") {
		parts = append(parts, "^"+regexp.QuoteMeta(p)+"$")
	}
	ctx, cancel := context.WithTimeout(t.Context(), pendingBound)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.v", "-test.count=1", "-test.run", strings.Join(parts, "/"))
	cmd.Env = append(os.Environ(), pendingBinEnv+"="+harnessBin)
	out, _ := cmd.CombinedOutput()
	switch {
	case bytes.Contains(out, []byte("--- PASS: "+t.Name()+" (")):
		t.Errorf("pending row now matches its engine golden; mark it same (was %s)", row)
	case bytes.Contains(out, []byte("--- FAIL: "+t.Name()+" (")):
		t.Skipf("%s\n%s", row, tail(out, 4096))
	default:
		t.Errorf("pending row did not run:\n%s", out)
	}
}

func tail(b []byte, n int) []byte {
	if len(b) > n {
		return b[len(b)-n:]
	}
	return b
}

// compareSame compares obs with the engine golden of row name.
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
	if w, g := string(mustJSON(t, want)), string(mustJSON(t, obs)); w != g {
		t.Errorf("runtime differs from %s (-golden +got):\n%s", path, lineDiff(w, g))
	}
}

// pendingCite matches a finding of the re-architecture review, which is not
// in the repository, or a phase.
var pendingCite = regexp.MustCompile(`^(F(0[1-9]|1\d|2[0-4])|phase \d)$`)

// citeErrors reports each cite of row that is neither a finding, a phase, nor
// a line of spec, and a cite list that the kind of row does not allow.
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
			errs = append(errs, fmt.Sprintf("cites %q, which is not a line of docs/architecture.md, a finding F01 to F24, or a phase", c))
		}
	}
	switch {
	case row.kind == rowSame && len(row.cites) > 0:
		errs = append(errs, "a same row cites nothing")
	case row.kind == rowRegolden && lines == 0:
		errs = append(errs, "a re-golden row cites at least one line of docs/architecture.md")
	case row.kind == rowDeleted && (lines != 1 || pending > 0):
		errs = append(errs, "a deleted row cites one line of docs/architecture.md")
	case row.kind == rowPending && len(row.cites) == 0:
		errs = append(errs, "a pending row cites a finding, a phase, or a line of docs/architecture.md")
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
	t.Parallel()
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

func TestCiteErrors(t *testing.T) {
	skipShort(t)
	t.Parallel()
	const spec = "line one\nline two"
	for _, tc := range []struct {
		name string
		row  runtimeRow
		bad  bool
	}{
		{"known finding", pendingOn("F24"), false},
		{"phase", pendingOn("phase 4"), false},
		{"unknown finding", pendingOn("F99"), true},
		{"finding zero", pendingOn("F00"), true},
		{"unowned", pendingOn("unowned"), true},
		{"spec line on a pending row", pendingOn("line two"), false},
		{"line not in the spec", reGolden("line three"), true},
		{"re-golden without a line", reGolden("F02"), true},
		{"pending without a cite", pendingOn(), true},
		{"same with a cite", runtimeRow{kind: rowSame, cites: []string{"line one"}}, true},
	} {
		if got := len(citeErrors(tc.row, spec)) > 0; got != tc.bad {
			t.Errorf("%s: citeErrors reports an error = %t, want %t", tc.name, got, tc.bad)
		}
	}
}
