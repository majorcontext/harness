package protocol

import "github.com/majorcontext/harness/config"

// ProviderSet is the body of PUT /providers: the whole map of configured provider entries. A missing providers is invalid.
type ProviderSet struct {
	Providers map[string]config.Provider `json:"providers"`
}

// ProviderStatus is the live state of one provider. Reason names why it has no backend, and holds no secret.
type ProviderStatus struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// Providers is the answer of PUT /providers: each provider of the live set, in name order.
type Providers struct {
	Providers []ProviderStatus `json:"providers"`
}
