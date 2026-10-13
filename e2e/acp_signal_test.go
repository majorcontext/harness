package e2e

import (
	"syscall"
	"testing"
)

// TestACPSignalEndsWithStatusZero pins the sentence of the ACP section on the
// exit status after SIGINT and SIGTERM.
func TestACPSignalEndsWithStatusZero(t *testing.T) {
	skipShort(t)
	t.Parallel()
	schema := loadACPSchema(t)
	for name, sig := range map[string]syscall.Signal{"SIGTERM": syscall.SIGTERM, "SIGINT": syscall.SIGINT} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in, out, g, stderr := startACPProc(t)
			c := newACPConn(t, schema, in, out)
			acpInit{version: 1, want: 1}.run(t, c)
			if err := g.cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			waitACPExit(t, g, stderr)
		})
	}
}
