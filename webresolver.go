package harness

import (
	"context"
	"net/netip"
)

// WebResolver resolves the host names of web_fetch, for tests that name a host
// with no DNS entry. A nil Options.WebResolver is the system resolver.
type WebResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}
