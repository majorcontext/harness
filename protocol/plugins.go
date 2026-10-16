package protocol

import "github.com/majorcontext/harness/config"

// PluginSet is the body of PUT /plugins: the whole set of config plugins. A missing plugins is invalid.
type PluginSet struct {
	Plugins []config.PluginSpec `json:"plugins"`
}

// Plugins is the answer of PUT /plugins: each plugin of the live set, in chain order.
type Plugins struct {
	Plugins []Plugin `json:"plugins"`
}
