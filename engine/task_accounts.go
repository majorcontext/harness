package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/majorcontext/harness/provider"
)

type AccountRoutingConfig struct {
	Vendor           string
	ProxyURLEnv      string
	Protocol         string
	ProxyURL         string
	ProxyURLResolver func() (string, error)
}

var subscriptionAccountIDPattern = regexp.MustCompile(`^acct_[A-Za-z0-9]{1,64}$`)

func (s *Session) accountSelectionSnapshot() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneAccountSelection(s.accountSelection)
}

func (s *Session) accountRoutingContext(ctx context.Context, providerName string) (context.Context, error) {
	selection := s.accountSelectionSnapshot()
	for vendor := range selection {
		if !routingSupportsVendor(s.cfg.AccountRouting, vendor) {
			return nil, fmt.Errorf("engine: account routing for selected vendor %q is not configured", vendor)
		}
	}
	route, ok := s.cfg.AccountRouting[providerName]
	if !ok {
		return ctx, nil
	}
	if _, ok := selection[route.Vendor]; !ok {
		return ctx, nil
	}
	if route.Protocol != "boxes-v1" {
		return nil, s.withSelectedAccountError(providerName, fmt.Errorf("engine: unsupported account-routing protocol"))
	}
	proxyBaseURL := route.ProxyURL
	if route.ProxyURLResolver != nil {
		var err error
		proxyBaseURL, err = route.ProxyURLResolver()
		if err != nil {
			return nil, s.withSelectedAccountError(providerName, fmt.Errorf("engine: account-routing environment variable %q is missing or invalid", route.ProxyURLEnv))
		}
	}
	if proxyBaseURL == "" {
		return nil, s.withSelectedAccountError(providerName, fmt.Errorf("engine: account-routing proxy URL is unavailable"))
	}
	proxyURL, err := accountProxyURL(proxyBaseURL, selection)
	if err != nil {
		return nil, s.withSelectedAccountError(providerName, fmt.Errorf("engine: invalid account-routing configuration for provider %q", providerName))
	}
	return provider.WithAccountRouting(ctx, provider.AccountRouting{ProxyURL: proxyURL}), nil
}

func (s *Session) withSelectedAccountError(providerName string, err error) error {
	if err == nil {
		return nil
	}
	if accountID := s.accountIDForProvider(providerName); accountID != nil {
		return fmt.Errorf("subscription account %q: %w", *accountID, err)
	}
	return err
}

func (s *Session) accountIDForProvider(providerName string) *string {
	route, ok := s.cfg.AccountRouting[providerName]
	if !ok {
		return nil
	}
	selection := s.accountSelectionSnapshot()
	id, ok := selection[route.Vendor]
	if !ok {
		return nil
	}
	return &id
}

func resolveTaskAccountSelection(raw json.RawMessage, modelProvider string, inherited map[string]string, routing map[string]AccountRoutingConfig) (map[string]string, error) {
	selection := cloneAccountSelection(inherited)
	if len(raw) == 0 {
		return selection, nil
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, fmt.Errorf("account must be a string or vendor map")
	}
	var account string
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &account); err != nil {
			return nil, fmt.Errorf("account must be a string or vendor map")
		}
		route, ok := routing[modelProvider]
		if !ok {
			return nil, fmt.Errorf("provider %q does not support account routing", modelProvider)
		}
		if !validSubscriptionAccountID(account) {
			return nil, fmt.Errorf("invalid subscription account id")
		}
		if selection == nil {
			selection = make(map[string]string)
		}
		selection[route.Vendor] = account
		return selection, nil
	}
	var requested map[string]json.RawMessage
	if err := json.Unmarshal(raw, &requested); err != nil || requested == nil {
		return nil, fmt.Errorf("account must be a string or vendor map")
	}
	for vendor, value := range requested {
		if vendor != "claude" && vendor != "codex" {
			return nil, fmt.Errorf("unsupported subscription account vendor %q", vendor)
		}
		if !routingSupportsVendor(routing, vendor) {
			return nil, fmt.Errorf("subscription account vendor %q is not configured", vendor)
		}
		var id string
		value = bytes.TrimSpace(value)
		if len(value) == 0 || value[0] != '"' || json.Unmarshal(value, &id) != nil {
			return nil, fmt.Errorf("account id for %q must be a string", vendor)
		}
		if !validSubscriptionAccountID(id) {
			return nil, fmt.Errorf("invalid subscription account id for %q", vendor)
		}
		if selection == nil {
			selection = make(map[string]string)
		}
		selection[vendor] = id
	}
	return selection, nil
}

func routingSupportsVendor(routing map[string]AccountRoutingConfig, vendor string) bool {
	for _, route := range routing {
		if route.Vendor == vendor {
			return true
		}
	}
	return false
}

func validSubscriptionAccountID(id string) bool {
	return id == "" || subscriptionAccountIDPattern.MatchString(id)
}

func cloneAccountSelection(selection map[string]string) map[string]string {
	if selection == nil {
		return nil
	}
	clone := make(map[string]string, len(selection))
	for vendor, id := range selection {
		clone[vendor] = id
	}
	return clone
}
