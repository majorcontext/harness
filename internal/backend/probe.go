package backend

import (
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/injectedauth"
	"github.com/majorcontext/harness/internal/provider/anthropic"
	responses "github.com/majorcontext/harness/internal/provider/openai"
)

// ProbeOf returns the status probe of the entry name with auth injected: the model list request of its wire. ok is false for
// an entry whose wire has none. The composition root of a connection gives it to injectedauth.New.
func ProbeOf(name string, p config.Provider) (injectedauth.Probe, bool) {
	switch config.WireOf(name, p) {
	case config.WireResponses:
		return injectedauth.Probe{URL: responses.ListURL(p.BaseURL, p.ResponsesPath, p.ModelsFrom == config.ModelsFromChatGPT)}, true
	case config.WireAnthropic:
		return injectedauth.Probe{URL: anthropic.ModelsURL(p.BaseURL), Headers: map[string]string{"Anthropic-Version": anthropic.Version}}, true
	}
	return injectedauth.Probe{}, false
}
