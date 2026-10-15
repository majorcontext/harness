package harness

import (
	"context"
	"crypto/x509"
	"net/netip"
	"net/url"
)

// WebTestSeam is a test seam of web_fetch that production code leaves zero.
type WebTestSeam struct {
	// AllowLoopback lets web_fetch reach 127.0.0.0/8 and ::1.
	AllowLoopback bool
	// Resolver resolves host names. nil: the system resolver.
	Resolver interface {
		LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
	}
	// Proxy replaces http.ProxyFromEnvironment. nil: the environment decides.
	Proxy func(*url.URL) (*url.URL, error)
	// RootCAs replaces the system roots of a request that goes through a proxy. nil: the system roots.
	RootCAs *x509.CertPool
}
