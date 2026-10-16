package cwdruntimes

import (
	"testing"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/acp"
)

func TestARuntimeOfAcpContinuesAfterAUsageLimitResets(t *testing.T) {
	o := New(Options{}).harnessOptions(&config.Config{}, t.TempDir(), acp.RuntimeOptions{})
	if !o.ContinueOnReset {
		t.Fatal("ContinueOnReset is false: an acp session would wait for the next input after a usage limit")
	}
}
