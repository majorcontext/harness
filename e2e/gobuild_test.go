package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// goBuild compiles the harness binary and the fakeclaude test fake from root
// into outDir. Only the harness binary takes the coverage flags, so the fake's
// statements stay out of the contract coverage total.
func goBuild(root, outDir string) error {
	out := outDir + string(filepath.Separator)
	var cover []string
	if os.Getenv("HARNESS_E2E_COVER") == "1" {
		cover = []string{"-cover", "-covermode=atomic", "-coverpkg=github.com/majorcontext/harness/..."}
	}
	for _, b := range []struct {
		pkg   string
		flags []string
	}{{"./harnesstest/fakeclaude", nil}, {"./cmd/harness", cover}} {
		cmd := exec.Command("go", append(append([]string{"build", "-o", out}, b.flags...), b.pkg)...)
		cmd.Dir = root
		if msg, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("go build %s: %v\n%s", b.pkg, err, msg)
		}
	}
	return nil
}

// binaries returns the binaries that the parent of a pending-row run built, or
// builds them.
func binaries() (string, func(), error) {
	if bin := os.Getenv(pendingBinEnv); bin != "" {
		return bin, func() {}, nil
	}
	return buildHarness()
}
