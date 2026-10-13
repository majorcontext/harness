package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

// acpStep is one action of an ACP scenario.
type acpStep interface {
	run(t *testing.T, c *acpConn)
}

// acpScenario is one row of the ACP table. cite is a sentence of the ACP
// section of docs/architecture.md that the row pins.
type acpScenario struct {
	name string
	cite string
	// model is the script of the model. config adds top-level keys to the user
	// config. dirs names extra directories and their files; "main" always
	// exists. allowed is the default tool set of the in-process adapter, so
	// a row that sets it runs on the hosts that take one: in process and Node wasm.
	model   []harnesstest.Step
	config  map[string]any
	dirs    map[string]map[string]string
	allowed []string
	// files are the files of the "main" directory.
	files map[string]string
	// wireOnly leaves the requests, the sessions, and the usage_update and session_info_update frames out of the golden, for a row whose turn depends on timing.
	wireOnly bool
	steps    []acpStep
	// needsExec marks a row that runs bash, a process, or git. needsMCP marks
	// one that passes an mcpServers entry. schedulerOrder marks one whose
	// frames interleave by goroutine scheduling, which the single thread of the
	// wasm module orders differently. The Node wasm host skips all three.
	needsExec, needsMCP, schedulerOrder bool
}

// acpInit sends initialize with the client protocol version and expects the
// agent to answer version want. notices adds the client capability session.notices.
type acpInit struct {
	version, want int
	notices       bool
}

// acpRaw writes one line as it is. A non-zero code expects an error response
// with a null id.
type acpRaw struct {
	line string
	code int
}

// acpCall sends a request and expects an error response with the code.
type acpCall struct {
	method string
	params any
	code   int
}

// acpNotify sends a notification, which has no response.
type acpNotify struct{ method string }

func (s acpInit) run(t *testing.T, c *acpConn) {
	t.Helper()
	caps := map[string]any{}
	if s.notices {
		caps["session"] = map[string]any{"notices": map[string]any{}}
	}
	id := c.request("initialize", map[string]any{
		"protocolVersion":    s.version,
		"clientCapabilities": caps,
		"clientInfo":         map[string]any{"name": "e2e", "version": "0"},
	})
	res := c.mustBeResult(c.response(id), "InitializeResponse")
	if got, _ := res["protocolVersion"].(float64); int(got) != s.want {
		t.Errorf("initialize with version %d answered version %v, want %d", s.version, res["protocolVersion"], s.want)
	}
}

func (s acpRaw) run(t *testing.T, c *acpConn) {
	t.Helper()
	c.raw(s.line)
	if s.code == 0 {
		return
	}
	f := c.read()
	if f["id"] != nil {
		t.Errorf("the response to %q has id %v, want null", s.line, f["id"])
	}
	c.mustBeError(f, s.code)
}

func (s acpCall) run(t *testing.T, c *acpConn) {
	t.Helper()
	c.mustBeError(c.response(c.request(s.method, s.params)), s.code)
}

func (s acpNotify) run(t *testing.T, c *acpConn) {
	t.Helper()
	c.send(map[string]any{"jsonrpc": "2.0", "method": s.method})
}

const (
	acpParseError     = -32700
	acpInvalidRequest = -32600
	acpMethodNotFound = -32601
	acpInvalidParams  = -32602
)

var acpScenarios = slices.Concat(acpWireScenarios, acpSessionScenarios, acpToolScenarios, acpConfigScenarios)

var acpWireScenarios = []acpScenario{
	{
		name: "initialize_answers_agent_info",
		cite: "`initialize` answers the agent info, the agent capabilities, and an empty `authMethods`.",
		steps: []acpStep{
			acpInit{version: 1, want: 1},
		},
	},
	{
		name: "initialize_newer_version_gets_latest",
		cite: "When the client version is not 1, the answer is version 1, the latest version of the agent.",
		steps: []acpStep{
			acpInit{version: 2, want: 1},
			acpInit{version: 0, want: 1},
		},
	},
	{
		name: "initialize_without_version_is_invalid",
		cite: "`initialize` without a `protocolVersion` that is an integer from 0 to 65535 is `-32602`.",
		steps: []acpStep{
			acpCall{method: "initialize", params: map[string]any{}, code: acpInvalidParams},
			acpCall{method: "initialize", params: map[string]any{"protocolVersion": 70000}, code: acpInvalidParams},
			acpInit{version: 1, want: 1},
		},
	},
	{
		name: "bad_json_keeps_connection_open",
		cite: "A line that is not JSON is answered with `-32700` and a null id, and the connection stays open.",
		steps: []acpStep{
			acpRaw{line: `{"jsonrpc":"2.0","id":1,"method":`, code: acpParseError},
			acpRaw{line: ``},
			acpRaw{line: `[1,2]`, code: acpInvalidRequest},
			acpInit{version: 1, want: 1},
		},
	},
	{
		name: "id_of_other_type_is_invalid",
		cite: "An `id` that is not a string, a number, or null is the same error.",
		steps: []acpStep{
			acpRaw{line: `{"jsonrpc":"2.0","id":true,"method":"initialize","params":{"protocolVersion":1}}`, code: acpInvalidRequest},
			acpRaw{line: `{"jsonrpc":"2.0","id":{"a":1},"method":"initialize","params":{"protocolVersion":1}}`, code: acpInvalidRequest},
			acpInit{version: 1, want: 1},
		},
	},
	{
		name: "request_with_null_id_is_answered",
		cite: "A request with a null `id` is a request and is answered.",
		steps: []acpStep{
			acpRaw{line: `{"jsonrpc":"2.0","id":null,"method":"bogus/method"}`, code: acpMethodNotFound},
		},
	},
	{
		name: "unknown_method_is_not_found",
		cite: "A request for a method that the agent lacks is answered with `-32601`; a notification for one has no answer.",
		steps: []acpStep{
			acpCall{method: "bogus/method", code: acpMethodNotFound},
			acpCall{method: "authenticate", params: map[string]any{"methodId": "x"}, code: acpMethodNotFound},
			acpNotify{method: "bogus/notification"},
			acpInit{version: 1, want: 1},
		},
	},
}

type acpGolden struct {
	Requests []normRequest            `json:"requests"`
	Sessions map[string][]normMessage `json:"sessions"`
	Wire     []acpFrame               `json:"wire"`
}

// acpObserve reads what the wire does not show: the normalized model requests,
// and the final transcript of each session, read from the store after the
// connection closed, with no ACP replay.
func acpObserve(t *testing.T, c *acpConn) acpGolden {
	t.Helper()
	st := store.NewDiskStore(c.env.sessDir)
	sessions := map[string][]transcriptMessage{}
	for alias, id := range c.sessions {
		v, err := harness.OpenView(t.Context(), st, id)
		if err != nil {
			t.Fatalf("view %s: %v", alias, err)
		}
		var msgs []transcriptMessage
		for before := uint64(0); ; {
			page, err := v.Messages(t.Context(), before, protocol.MaxMessageLimit)
			if err != nil {
				t.Fatal(err)
			}
			msgs = append(transcriptOfPage(t, page.Messages), msgs...)
			if !page.HasMore {
				break
			}
			before = page.FirstSeq
		}
		sessions[alias] = msgs
	}
	obs := normalizeRun(c.fake.Requests(), sessions, nil, c.sessions, c.dirs["main"])
	return acpGolden{Requests: obs.Requests, Sessions: obs.Sessions, Wire: c.goldenWire()}
}

func acpGoldenOf(t *testing.T, c *acpConn, wireOnly bool) acpGolden {
	t.Helper()
	if wireOnly {
		return acpGolden{Wire: acpWithoutTurnUpdates(c.goldenWire())}
	}
	return acpObserve(t, c)
}

func acpHosts() []acpHost {
	return []acpHost{
		{name: "binary", start: startACPBinary},
		{name: "in-process", start: startACPInProcess},
	}
}

// acpTableHosts adds the Node wasm host when HARNESS_E2E_NODE=1.
func acpTableHosts() []acpHost {
	hosts := acpHosts()
	if os.Getenv(nodeEnv) == "1" {
		hosts = append(hosts, acpHost{name: nodeHostName, start: startACPNode})
	}
	return hosts
}

// runsOn says whether the row runs on the host. A row with a default tool set
// runs on the hosts that take one.
func (sc acpScenario) runsOn(host string) bool {
	switch host {
	case "binary":
		return len(sc.allowed) == 0
	case nodeHostName:
		return !sc.needsExec && !sc.needsMCP && !sc.schedulerOrder
	}
	return true
}

// TestACPScenarios runs each row on the binary host and on the in-process host,
// and with HARNESS_E2E_NODE=1 on the Node wasm host. The hosts must produce the
// one golden.
func TestACPScenarios(t *testing.T) {
	skipShort(t)
	t.Parallel()
	schema := loadACPSchema(t)
	for _, sc := range acpScenarios {
		for _, host := range acpTableHosts() {
			if !sc.runsOn(host.name) {
				continue
			}
			t.Run(sc.name+"/"+host.name, func(t *testing.T) {
				t.Parallel()
				fake := harnesstest.NewAnthropic(t, sc.model...)
				env := acpEnv{configPath: writeGoalConfigWith(t, fake.URL(), sc.config), sessDir: t.TempDir(), allowed: sc.allowed}
				in, out, stop := host.start(t, env)
				c := newACPConn(t, schema, in, out)
				c.fake, c.env = fake, env
				c.dirs["main"] = writeACPDir(t, sc.files)
				for name, files := range sc.dirs {
					c.dirs[name] = writeACPDir(t, files)
				}
				for _, step := range sc.steps {
					step.run(t, c)
				}
				c.finish(stop)
				update := *updateGoldens && (host.name == "binary" || len(sc.allowed) > 0 && host.name == "in-process")
				got := acpGoldenOf(t, c, sc.wireOnly)
				if host.name == nodeHostName {
					got = alignNodeGolden(t, got)
				}
				compareACPGolden(t, filepath.Join("testdata", "acp", sc.name+".json"), got, update)
			})
		}
	}
}

func compareACPGolden(t *testing.T, path string, got acpGolden, update bool) {
	t.Helper()
	b, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	if update {
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v (run with -update to create it)", err)
	}
	if string(want) != string(b) {
		t.Errorf("wire differs from %s (-golden +got):\n%s", path, lineDiff(string(want), string(b)))
	}
}

// TestACPRows checks that each ACP scenario has a golden and cites a sentence
// of the ACP section of the spec, and that each golden has a scenario.
func TestACPRows(t *testing.T) {
	skipShort(t)
	t.Parallel()
	spec, err := os.ReadFile(filepath.Join("..", "docs", "architecture.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(spec), "\n## ACP\n")
	if !ok {
		t.Fatal("docs/architecture.md has no ACP section")
	}
	section, _, _ := strings.Cut(rest, "\n## ")
	names := map[string]bool{}
	for _, sc := range acpScenarios {
		names[sc.name] = true
		if sc.cite == "" || !strings.Contains(section, sc.cite) {
			t.Errorf("row %s cites %q, which the ACP section does not hold", sc.name, sc.cite)
		}
		if _, err := os.Stat(filepath.Join("testdata", "acp", sc.name+".json")); err != nil {
			t.Errorf("row %s has no golden: %v", sc.name, err)
		}
	}
	goldens, err := filepath.Glob(filepath.Join("testdata", "acp", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range goldens {
		name := strings.TrimSuffix(filepath.Base(g), ".json")
		if !names[name] {
			t.Errorf("golden %s has no ACP row", g)
		}
	}
	if len(acpScenarios) == 0 || !slices.ContainsFunc(acpScenarios, func(s acpScenario) bool { return len(s.steps) > 0 }) {
		t.Error("the ACP table has no row with a step")
	}
}
