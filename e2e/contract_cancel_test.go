package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/internal/testpoll"
)

// fileHolds waits until the file in the work directory holds want.
type fileHolds struct{ path, want string }

func (a fileHolds) run(t *testing.T, r *run) {
	t.Helper()
	path := filepath.Join(r.drv.Workdir(), a.path)
	testpoll.Until(t, waitBound, a.path+" never held "+a.want, func() bool {
		data, err := os.ReadFile(path)
		return err == nil && strings.TrimSpace(string(data)) == a.want
	}, 15*time.Millisecond)
}

// cancelRows are the runtime dispositions of the rows of this file.
var cancelRows = map[string]runtimeRow{
	"claudecode_cli_that_cancels_a_tool_request_stops_the_call":  sameAsServe(),
	"claudecode_cli_that_drops_a_tool_connection_stops_the_call": sameAsServe(),
	"claudecode_interrupt_stops_a_running_tool_call":             sameAsServe(),
	"interrupt_stops_a_running_bash_call":                        sameAsServe(),
	"file_tools_refuse_a_named_pipe":                             sameAsServe(),
}


func TestContractToolCallCancel(t *testing.T) {
	runScenarios(t, []scenario{{
		name:    "claudecode_cli_that_cancels_a_tool_request_stops_the_call",
		driver:  claudeLane{mode: "abandon_tool"}.newDriver,
		actions: withActions(claudeOneTurn, fileHolds{path: "abandon.reply", want: "no reply"}),
	}, {
		name:    "claudecode_cli_that_drops_a_tool_connection_stops_the_call",
		driver:  claudeLane{mode: "drop_tool"}.newDriver,
		actions: claudeOneTurn,
	}, {
		name:   "claudecode_interrupt_stops_a_running_tool_call",
		driver: claudeLane{mode: "hold_tool", signals: true}.newDriver,
		actions: []action{create{as: "a"}, submit{as: "a", text: "run it"}, awaitFile{path: "abandon.pid"},
			interrupt{as: "a"}, waitIdle{as: "a"}, claudeSession{as: "a"}},
	}, {
		name:  "interrupt_stops_a_running_bash_call",
		model: toolChain(ftBash("touch tool-running; sleep 600"))[:1],
		actions: []action{create{as: "a"}, submit{as: "a", text: "go"}, awaitFile{path: "tool-running"},
			interrupt{as: "a"}, waitIdle{as: "a"}},
	}})
}
