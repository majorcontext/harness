package gates

import (
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/modelmeta"
)

func TestSubscriptionDefaultModelsAreListed(t *testing.T) {
	for provider, ref := range map[string]string{"codex": config.SubscriptionCodexModel, "claude-code": config.SubscriptionClaudeModel} {
		id := strings.TrimPrefix(ref, provider+"/")
		if id == ref || !slices.ContainsFunc(modelmeta.Models(provider), func(m modelmeta.Model) bool { return m.ID == id }) {
			t.Errorf("modelmeta.Models(%q) does not list %q, the subscription default %q", provider, id, ref)
		}
	}
}
