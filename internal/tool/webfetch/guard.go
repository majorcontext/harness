package webfetch

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
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

// router sends each request one of two ways. With a proxy that the
// environment names for the URL, it checks the host as far as it can before the
// send, and the proxy owns the dial. Without one, every dial goes to a public
// address. The choice is per request, so each redirect hop makes it again.
type router struct {
	direct, proxied http.RoundTripper
	proxy           func(*http.Request) (*url.URL, error)
	res             resolver
	loopback        bool
}

func (r *router) RoundTrip(req *http.Request) (*http.Response, error) {
	pu, err := r.proxy(req)
	if err != nil {
		return nil, err
	}
	if pu == nil {
		return r.direct.RoundTrip(req)
	}
	if err := precheck(req.Context(), req.URL.Hostname(), r.res, r.loopback); err != nil {
		return nil, err
	}
	return r.proxied.RoundTrip(req)
}

// precheck refuses a literal IP that is not public, and a host name with any
// resolved address that is not public. A name that the lookup reports as not
// found (NXDOMAIN) passes, because the proxy may resolve names that this host
// cannot. Any other lookup error, such as a refused connection or a timeout,
// refuses the request.
func precheck(ctx context.Context, host string, r resolver, loopback bool) error {
	if ip, err := netip.ParseAddr(host); err == nil {
		return checkAll([]netip.Addr{ip}, host, loopback)
	}
	ips, err := r.LookupNetIP(ctx, "ip", host)
	if err != nil {
		if noDNS(err) {
			return nil
		}
		return err
	}
	return checkAll(ips, host, loopback)
}

// noDNS reports a lookup that answered that the name does not exist.
func noDNS(err error) bool {
	de, ok := errors.AsType[*net.DNSError](err)
	return ok && de.IsNotFound
}

func checkAll(ips []netip.Addr, host string, loopback bool) error {
	for _, ip := range ips {
		if !public(ip, loopback) {
			return &notPublicError{host: host}
		}
	}
	return nil
}

// newTransport returns the router of a tool. The direct transport reads no
// proxy variable and dials public addresses only. The proxied transport trusts
// the system roots, as the other clients do, so a proxy that intercepts TLS and
// has its CA in that pool works; no option skips the verification. The proxy
// comes from http.ProxyFromEnvironment, like the proxy of the other clients.
func newTransport(o Options) http.RoundTripper {
	var res resolver = net.DefaultResolver
	if o.Test.Resolver != nil {
		res = o.Test.Resolver
	}
	proxy := http.ProxyFromEnvironment
	if o.Test.Proxy != nil {
		proxy = func(req *http.Request) (*url.URL, error) { return o.Test.Proxy(req.URL) }
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	base := func() *http.Transport {
		return &http.Transport{
			ForceAttemptHTTP2:      true,
			DisableKeepAlives:      true,
			TLSHandshakeTimeout:    10 * time.Second,
			MaxResponseHeaderBytes: 1 << 20,
		}
	}
	direct := base()
	direct.DialContext = guardedDial(d.DialContext, res, o.Test.AllowLoopback)
	proxied := base()
	proxied.DialContext = d.DialContext
	proxied.Proxy = proxy
	if o.Test.RootCAs != nil {
		proxied.TLSClientConfig = &tls.Config{RootCAs: o.Test.RootCAs}
	}
	return &router{direct: direct, proxied: proxied, proxy: proxy, res: res, loopback: o.Test.AllowLoopback}
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
		if err := checkAll(ips, host, loopback); err != nil {
			return nil, err
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
