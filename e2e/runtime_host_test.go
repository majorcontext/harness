package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
	awaitToolCall(t *testing.T, id, callID string)
	awaitAdmitted(t *testing.T, id, text string, n int)
	resolveQuestion(t *testing.T, id, callID string, res resolution) callResult
	sendFrom(t *testing.T, id, text, source, delivery string) callResult
	journalEvents(t *testing.T, id, prefix string) []any
	turnEnds(t *testing.T, id string) []any
	backendStateKeys(t *testing.T, id string) callResult
	compactKeeping(t *testing.T, id string, keep int) callResult
}

var (
	serveHost = host{openIn: func(t *testing.T, configPath, workDir string, env map[string]string, args ...string) laneHost {
		return newServeDriverIn(t, configPath, env, workDir, args...)
	}}
	// runtimeHost reads env through the Getenv of its runtime, not the process.
	runtimeHost = host{runtime: true, openIn: func(t *testing.T, configPath, workDir string, env map[string]string, args ...string) laneHost {
		ask := slices.Contains(args, "--ask-user-question")
		if len(args) > 1 || len(args) == 1 && !ask {
			t.Fatalf("the runtime takes only the serve flag --ask-user-question: %q", args)
		}
		return newRuntimeDriverIn(t, configPath, ask, workDir, env)
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
	// rowSame: the observation equals the serve golden under testdata/contract.
	rowSame rowKind = iota
	// rowRegolden: the runtime observation has its own golden under
	// testdata/runtime, and the cites account for each difference.
	rowRegolden
	// rowDeleted: the spec deletes what the row pins. It does not run.
	rowDeleted
	// rowPending: the row waits for a fix. It runs as an
	// expected failure.
	rowPending
	// rowRuntimeOnly: only the runtime host runs the row, because its
	// determinism needs an in-process seam. The contract golden is the
	// golden of the runtime host.
	rowRuntimeOnly
)

// runtimeRow is the disposition of one row. cites are the lines of
// the sentences of docs/architecture.md and the reason ids of
// testdata/runtime-reasons.txt that the disposition rests on.
type runtimeRow struct {
	kind  rowKind
	cites []string
}

func (r runtimeRow) String() string {
	return fmt.Sprintf("%s: %s", [...]string{"same", "re-golden", "deleted by design", "pending", "runtime only"}[r.kind], strings.Join(r.cites, "; "))
}

// runtimeEnv enables the runtime host, which runs beside the serve host.
const runtimeEnv = "HARNESS_E2E_RUNTIME"

// onHost runs each row of table on h by its disposition. The runtime host
// runs only with runtimeEnv, and under -update only the runtime-only rows,
// which the serve host does not run. Every row owns its environment, so
// every row runs in parallel.
func onHost[S any](t *testing.T, h host, table []S, key func(S) string, run func(*testing.T, S) observation) {
	t.Helper()
	if h.runtime && os.Getenv(runtimeEnv) == "" && !*updateGoldens {
		return
	}
	t.Run(h.name(), func(t *testing.T) {
		for _, sc := range table {
			name := key(sc)
			t.Run(name, func(t *testing.T) {
				row, ok := rowDisposition(name)
				if !ok {
					t.Fatalf("row %s has no runtime disposition", name)
				}
				if row.kind == rowDeleted {
					t.Skip(row)
				}
				only := row.kind == rowRuntimeOnly
				if !h.runtime && only {
					t.Skip(row)
				}
				if h.runtime && *updateGoldens && !only {
					t.Skip("-update writes this row from the serve host")
				}
				pending := row.kind == rowPending
				if pending && os.Getenv(pendingBinEnv) == "" {
					expectPendingFailure(t, row)
					return
				}
				if !pending {
					t.Parallel()
				}
				obs := run(t, sc)
				if row.kind == rowSame && !h.runtime && !pending {
					compareGoldenAt(t, filepath.Join("testdata", "contract", name+".golden.json"), obs, *updateGoldens)
					return
				}
				if only && *updateGoldens {
					compareGoldenAt(t, filepath.Join("testdata", "contract", name+".golden.json"), obs, true)
					return
				}
				if row.kind == rowSame || row.kind == rowRuntimeOnly || pending {
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
// phase has made it match the serve golden, and it must become a same row.
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
	if bytes.Contains(out, []byte("response breaks the served contract")) {
		t.Errorf("pending row %s breaks the served contract:\n%s", row, tail(out, 4096))
	}
	switch {
	case t.Failed():
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

// compareSame compares obs with the serve golden of row name.
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

// reasonsFile lists the closed set of reasons that a row may cite when the
// doc cannot hold them: one line of "<id> <reason>" for each.
const reasonsFile = "testdata/runtime-reasons.txt"

func loadReasons(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(reasonsFile)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		id, text, ok := strings.Cut(line, " ")
		if !ok || id == "" || strings.TrimSpace(text) == "" {
			t.Fatalf("%s: line %q is not \"<id> <reason>\"", reasonsFile, line)
		}
		if _, dup := out[id]; dup {
			t.Fatalf("%s: id %q repeats", reasonsFile, id)
		}
		out[id] = text
	}
	return out
}

// citeErrors reports each cite that is neither a sentence of spec nor a
// reason id of reasons, and each cite list that the kind of row does not allow.
func citeErrors(row runtimeRow, spec string, reasons map[string]string) []string {
	var errs []string
	for _, c := range row.cites {
		_, listed := reasons[c]
		if c == "" || (!listed && !strings.Contains(spec, c)) {
			errs = append(errs, fmt.Sprintf("cites %q, which is neither text of docs/architecture.md nor an id of %s", c, reasonsFile))
		}
	}
	switch {
	case row.kind == rowSame && len(row.cites) > 0:
		errs = append(errs, "a same row cites nothing")
	case row.kind == rowRegolden && len(row.cites) == 0:
		errs = append(errs, "a re-golden row cites at least one sentence of docs/architecture.md or one reason id")
	case row.kind == rowDeleted && len(row.cites) != 1:
		errs = append(errs, "a deleted row cites one sentence of docs/architecture.md or one reason id")
	case row.kind == rowRuntimeOnly && (len(row.cites) != 1 || !listed(reasons, row.cites[0])):
		errs = append(errs, "a runtime-only row cites one reason id of "+reasonsFile)
	case row.kind == rowPending && len(row.cites) == 0:
		errs = append(errs, "a pending row cites a sentence of docs/architecture.md or a reason id")
	}
	return errs
}

func listed(reasons map[string]string, id string) bool {
	_, ok := reasons[id]
	return ok
}

// testStringLiterals holds each row-name string literal of the e2e test files
// except the dispositions. A row name also reaches a table through a helper
// call, so no composite literal check finds every name.
func testStringLiterals(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, file := range files {
		if file == "runtime_rows_test.go" {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING {
				if s, err := strconv.Unquote(bl.Value); err == nil && rowNamePattern.MatchString(s) {
					out[s] = true
				}
			}
			return true
		})
	}
	return out
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

// TestRuntimeRows checks that each golden has one disposition, that a same or
// pending row has a contract golden, that a re-golden row has a runtime golden
// only, that a deleted row has no golden, and that each citation holds.
func TestRuntimeRows(t *testing.T) {
	skipShort(t)
	t.Parallel()
	spec, err := os.ReadFile(filepath.Join("..", "docs", "architecture.md"))
	if err != nil {
		t.Fatal(err)
	}
	reasons := loadReasons(t)
	used := map[string]bool{}
	serve, runtime := goldenNames(t, "contract"), goldenNames(t, "runtime")
	scenarios := testStringLiterals(t)
	for _, name := range serve {
		if _, ok := rowDisposition(name); !ok {
			t.Errorf("golden %s has no runtime disposition", name)
		}
	}
	for name, row := range allRowDispositions() {
		if _, ok := scenarios[name]; !ok {
			t.Errorf("row %s names no scenario", name)
		}
		hasServe, hasRuntime := slices.Contains(serve, name), slices.Contains(runtime, name)
		wantServe := row.kind == rowSame || row.kind == rowPending || row.kind == rowRuntimeOnly
		switch {
		case wantServe && !hasServe:
			t.Errorf("row %s is %s and needs a contract golden", name, row)
		case !wantServe && hasServe:
			t.Errorf("row %s is %s and has a contract golden that it does not use", name, row)
		}
		switch {
		case row.kind == rowRegolden && !hasRuntime:
			t.Errorf("row %s is %s and needs a runtime golden", name, row)
		case row.kind != rowRegolden && hasRuntime:
			t.Errorf("row %s is %s and has a runtime golden that it does not use", name, row)
		}
		for _, c := range row.cites {
			used[c] = true
		}
		for _, e := range citeErrors(row, string(spec), reasons) {
			t.Errorf("row %s: %s", name, e)
		}
	}
	for id := range reasons {
		if !used[id] {
			t.Errorf("%s lists %q, which no row cites", reasonsFile, id)
		}
	}
	for _, name := range runtime {
		if _, ok := rowDisposition(name); !ok {
			t.Errorf("runtime golden %s has no disposition", name)
		}
	}
}

func TestCiteErrors(t *testing.T) {
	skipShort(t)
	t.Parallel()
	const spec = "line one\nline two"
	reasons := map[string]string{"gone": "the runtime has no such route"}
	for _, tc := range []struct {
		name string
		row  runtimeRow
		bad  bool
	}{
		{"sentence of the spec", reGolden("line two"), false},
		{"reason id", pendingOn("gone"), false},
		{"deleted by a reason id", deletedBy("gone"), false},
		{"text not in the spec", reGolden("line three"), true},
		{"unlisted id", deletedBy("missing"), true},
		{"empty cite", reGolden(""), true},
		{"re-golden without a cite", reGolden(), true},
		{"deleted with two cites", runtimeRow{kind: rowDeleted, cites: []string{"gone", "line one"}}, true},
		{"pending without a cite", pendingOn(), true},
		{"same with a cite", runtimeRow{kind: rowSame, cites: []string{"line one"}}, true},
	} {
		if got := len(citeErrors(tc.row, spec, reasons)) > 0; got != tc.bad {
			t.Errorf("%s: citeErrors reports an error = %t, want %t", tc.name, got, tc.bad)
		}
	}
}
