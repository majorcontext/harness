package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ProxyURL returns the proxy URL of account: the URL in the variable ProxyURLEnv, with Separator and account added to its username.
// The password, host, and other parts stay as they are. An error never holds the URL, because it can hold a password.
func (r AccountRouting) ProxyURL(getenv func(string) string, account string) (*url.URL, error) {
	switch {
	case r.ProxyURLEnv == "" || r.Separator == "":
		return nil, errors.New("account_routing requires proxy_url_env and separator")
	case account == "":
		return nil, errors.New("account routing needs an account ID")
	case getenv == nil:
		return nil, fmt.Errorf("account routing cannot read %s: no Getenv is set", r.ProxyURLEnv)
	}
	raw := getenv(r.ProxyURLEnv)
	if raw == "" {
		return nil, fmt.Errorf("account routing: %s is empty", r.ProxyURLEnv)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return nil, fmt.Errorf("account routing: %s is not a proxy URL with a host", r.ProxyURLEnv)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("account routing: %s has an unsupported proxy scheme %q", r.ProxyURLEnv, u.Scheme)
	}
	if u.User == nil || u.User.Username() == "" {
		return nil, fmt.Errorf("account routing: %s has no username to qualify", r.ProxyURLEnv)
	}
	name := u.User.Username() + r.Separator + account
	if pw, ok := u.User.Password(); ok {
		u.User = url.UserPassword(name, pw)
	} else {
		u.User = url.User(name)
	}
	return u, nil
}

// NoProxyBypass returns an error that names the first target that a proxy of the environment would not carry:
// a loopback host, or a host that noProxy lists. noProxy has the format of NO_PROXY: a comma-separated list of "*",
// IP addresses, CIDR blocks, domain names (a leading dot matches subdomains only), and host:port pairs.
func NoProxyBypass(noProxy string, targets ...*url.URL) error {
	for _, t := range targets {
		if t != nil && bypassed(noProxy, t) {
			return fmt.Errorf("account routing: %s would bypass the proxy (loopback or NO_PROXY), so the call would not carry the account", t.Hostname())
		}
	}
	return nil
}

func bypassed(noProxy string, t *url.URL) bool {
	host := strings.ToLower(t.Hostname())
	ip := net.ParseIP(host)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || (ip != nil && ip.IsLoopback()) {
		return true
	}
	port := t.Port()
	if port == "" {
		port = map[string]string{"http": "80", "ws": "80", "https": "443", "wss": "443"}[t.Scheme]
	}
	for entry := range strings.SplitSeq(strings.ToLower(noProxy), ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if entry == "*" {
			return true
		}
		if _, cidr, err := net.ParseCIDR(entry); err == nil {
			if ip != nil && cidr.Contains(ip) {
				return true
			}
			continue
		}
		pattern, wantPort := entry, ""
		if h, p, err := net.SplitHostPort(entry); err == nil {
			pattern, wantPort = h, p
		}
		if wantPort != "" && wantPort != port {
			continue
		}
		pattern = strings.TrimPrefix(pattern, "*")
		switch {
		case strings.HasPrefix(pattern, "."):
			if strings.HasSuffix(host, pattern) {
				return true
			}
		case host == pattern || strings.HasSuffix(host, "."+pattern):
			return true
		}
	}
	return false
}
