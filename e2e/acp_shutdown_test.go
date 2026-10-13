package e2e

import (
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

// TestACPSessionNewRacesEndOfInput closes the input right after session/new.
// The agent answers with a session or with session_not_owned, and exits with no
// session left running.
func TestACPSessionNewRacesEndOfInput(t *testing.T) {
	skipShort(t)
	t.Parallel()
	schema := loadACPSchema(t)
	for _, host := range acpHosts() {
		t.Run(host.name, func(t *testing.T) {
			t.Parallel()
			for range 10 {
				fake := harnesstest.NewAnthropic(t)
				env := acpEnv{configPath: writeGoalConfigWith(t, fake.URL(), nil), sessDir: t.TempDir()}
				in, out, stop := host.start(t, env)
				c := newACPConn(t, schema, in, out)
				c.dirs["main"] = t.TempDir()
				id := c.request("session/new", map[string]any{"cwd": c.dir(""), "mcpServers": []any{}})
				acpCloseInput{}.run(t, c)
				f := c.response(id)
				if _, ok := f["result"]; !ok {
					c.mustBeError(f, acpInternal)
				} else {
					// The commands update follows the answer, and the end of input can come first.
					for l := range c.lines {
						if l.text != "" && !strings.Contains(l.text, "available_commands_update") {
							t.Errorf("stdout holds an unexpected line: %q", l.text)
						}
					}
				}
				c.finish(stop)
			}
		})
	}
}
