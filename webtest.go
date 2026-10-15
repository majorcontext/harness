package harness

import (
	"context"
	"crypto/x509"
	"net/netip"
	"net/url"
)

// WebTestSeam is a test seam of web_fetch. The config has no such key, and no
// production code sets it. The zero value changes nothing.
type WebTestSeam struct {
	// AllowLoopback lets web_fetch reach 127.0.0.0/8 and ::1.
	AllowLoopback bool
	Resolver      interface {
		LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
	}
	// Proxy replaces http.ProxyFromEnvironment. nil: the environment decides.
	Proxy func(*url.URL) (*url.URL, error)
	// RootCAs replaces the system roots of a request that goes through a proxy. nil: the system roots.
	RootCAs *x509.CertPool
}
