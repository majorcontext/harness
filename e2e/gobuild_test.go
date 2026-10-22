package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// goBuild compiles the harness binary and the fakeclaude test fake from root
// into outDir. Only the harness binary takes the coverage flags, so the fake's
// statements stay out of the contract coverage total. With coverage, the
// covered binary is harness-covered and harness is coverwrap, which gives
// each process its own coverage directory.
func goBuild(root, outDir string) error {
	out := outDir + string(filepath.Separator)
	type build struct {
		pkg, name string
		flags     []string
	}
	builds := []build{{"./harnesstest/fakeclaude", "fakeclaude", nil}, {"./cmd/harness", "harness", nil}}
	if os.Getenv("HARNESS_E2E_COVER") == "1" {
		builds[1] = build{"./cmd/harness", "harness-covered", []string{"-cover", "-covermode=atomic", "-coverpkg=github.com/majorcontext/harness/..."}}
		builds = append(builds, build{"./harnesstest/coverwrap", "harness", nil})
	}
	for _, b := range builds {
		cmd := exec.Command("go", append(append([]string{"build", "-o", out + b.name}, b.flags...), b.pkg)...)
		cmd.Dir = root
		if msg, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("go build %s: %v\n%s", b.pkg, err, msg)
		}
	}
	return nil
}

// binaries returns the binaries that the parent of a pending-row run built, or
// builds them. A child of a pending-row run also takes the shorter wait bound.
func binaries() (string, func(), error) {
	if bin := os.Getenv(pendingBinEnv); bin != "" {
		waitBound = pendingWaitBound
		return bin, func() {}, nil
	}
	return buildHarness()
}
