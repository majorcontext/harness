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
// in process.
type host struct {
	name      string
	newDriver func(t *testing.T, modelURL string, config map[string]any) driver
}

var (
	serveHost = host{"serve", func(t *testing.T, modelURL string, config map[string]any) driver {
		return newHTTPDriverWith(t, modelURL, config)
	}}
	runtimeHost = host{"runtime", func(t *testing.T, modelURL string, config map[string]any) driver {
		return newRuntimeDriver(t, modelURL, config)
	}}
)

// rowKind is how a contract row maps to the runtime.
type rowKind int

const (
	// rowSame: the runtime observation equals the serve golden.
	rowSame rowKind = iota
	// rowRegolden: the runtime observation differs by design and has its own
	// golden under testdata/runtime.
	rowRegolden
	// rowDeleted: the spec deletes what the row pins. It does not run.
	rowDeleted
	// rowPending: the row waits for a fix or a later phase. It does not run.
	rowPending
)

// runtimeRow is the disposition of one row. cite is a line of
// docs/architecture.md for rowRegolden and rowDeleted, and the finding IDs or
// phases that a pending row waits for.
type runtimeRow struct {
	kind rowKind
	cite string
}

func (r runtimeRow) String() string {
	return fmt.Sprintf("%s: %s", [...]string{"same", "re-golden by design", "deleted by design", "pending"}[r.kind], r.cite)
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

var pendingCite = regexp.MustCompile(`^(F\d\d|phase \d|unowned)(, (F\d\d|phase \d|unowned))*$`)

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
		case row.kind == rowPending && !pendingCite.MatchString(row.cite):
			t.Errorf("pending row %s cites %q, want finding IDs, phases, or unowned", name, row.cite)
		case (row.kind == rowRegolden || row.kind == rowDeleted) && !strings.Contains(string(spec), row.cite):
			t.Errorf("row %s cites %q, which docs/architecture.md does not hold", name, row.cite)
		}
	}
	for _, name := range runtime {
		if _, ok := runtimeRows[name]; !ok {
			t.Errorf("runtime golden %s has no disposition", name)
		}
	}
}
