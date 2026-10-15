package webfetch

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// notPublicError reports a host with an address that web_fetch refuses.
type notPublicError struct{ host string }

func (e *notPublicError) Error() string {
	return fmt.Sprintf("refused: %s is not a public address", e.host)
}

// blocked lists the ranges that the std netip predicates do not cover. The
// predicates cover loopback, private, link-local, multicast, and unspecified
// addresses.
var blocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("fec0::/10"),
}

// embedsV4 lists the IPv6 ranges that carry an IPv4 address: the offset of
// that address in the 16 bytes, by prefix.
var embedsV4 = []struct {
	prefix netip.Prefix
	offset int
}{
	{netip.MustParsePrefix("64:ff9b::/96"), 12},
	{netip.MustParsePrefix("2002::/16"), 2},
}

// public reports whether a client may dial addr. loopback lets 127.0.0.0/8 and
// ::1 pass, for tests.
func public(addr netip.Addr, loopback bool) bool {
	addr = addr.Unmap().WithZone("")
	if !addr.IsValid() {
		return false
	}
	if addr.IsLoopback() {
		return loopback
	}
	if addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}
	for _, p := range blocked {
		if p.Contains(addr) {
			return false
		}
	}
	if addr.Is6() {
		raw := addr.As16()
		for _, e := range embedsV4 {
			if e.prefix.Contains(addr) {
				return public(netip.AddrFrom4([4]byte(raw[e.offset:e.offset+4])), loopback)
			}
		}
	}
	return true
}

// newTransport returns a transport whose every dial goes to a public address.
// It reads no proxy variable: a proxy would dial for the client and skip the
// check.
func newTransport(loopback bool) *http.Transport {
	d := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Transport{
		Proxy:                  nil,
		DialContext:            guardedDial(d.DialContext, net.DefaultResolver, loopback),
		ForceAttemptHTTP2:      true,
		DisableKeepAlives:      true,
		TLSHandshakeTimeout:    10 * time.Second,
		MaxResponseHeaderBytes: 1 << 20,
	}
}

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

type resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// guardedDial resolves the host of addr, refuses it when one resolved address
// is not public, and dials a checked address itself. The connection can then
// never go to an address that the check did not see, so a second DNS answer
// cannot bypass it.
func guardedDial(dial dialFunc, r resolver, loopback bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		var ips []netip.Addr
		if ip, err := netip.ParseAddr(host); err == nil {
			ips = []netip.Addr{ip}
		} else if ips, err = r.LookupNetIP(ctx, "ip", host); err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("no address for %s", host)
		}
		for _, ip := range ips {
			if !public(ip, loopback) {
				return nil, &notPublicError{host: host}
			}
		}
		var last error
		for _, ip := range ips {
			c, err := dial(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
			if err == nil {
				return c, nil
			}
			last = err
			if ctx.Err() != nil {
				break
			}
		}
		return nil, last
	}
}
