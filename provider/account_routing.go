package provider

import (
	"errors"
	"net/url"
	"strings"

	"golang.org/x/net/http/httpproxy"
)

func AccountRoutingProxyFunc(raw string) (func(*url.URL) (*url.URL, error), error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User == nil {
		return nil, errors.New("provider: invalid account-routing proxy URL")
	}
	username := parsed.User.Username()
	password, hasPassword := parsed.User.Password()
	if !hasPassword || password == "" || strings.Contains(username, ":") {
		return nil, errors.New("provider: invalid account-routing proxy credentials")
	}
	config := httpproxy.FromEnvironment()
	config.HTTPProxy = raw
	config.HTTPSProxy = raw
	config.CGI = false
	proxyFunc := config.ProxyFunc()
	return func(target *url.URL) (*url.URL, error) {
		proxy, err := proxyFunc(target)
		if err != nil {
			return nil, errors.New("provider: account-routing proxy selection failed")
		}
		if proxy == nil {
			return nil, errors.New("provider: account-routing endpoint is excluded by NO_PROXY")
		}
		return proxy, nil
	}, nil
}

func ValidateAccountRoutingTarget(proxyURL, targetURL string) error {
	proxyFunc, err := AccountRoutingProxyFunc(proxyURL)
	if err != nil {
		return err
	}
	target, err := url.Parse(targetURL)
	if err != nil || target == nil || (target.Scheme != "http" && target.Scheme != "https") || target.Hostname() == "" {
		return errors.New("provider: invalid account-routing target")
	}
	_, err = proxyFunc(target)
	return err
}
