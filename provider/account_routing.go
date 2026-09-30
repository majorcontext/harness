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

func CodexAccountRoutingProxyFunc(raw string) (func(*url.URL) (*url.URL, error), error) {
	proxyFunc, err := AccountRoutingProxyFunc(raw)
	if err != nil {
		return nil, err
	}
	return func(target *url.URL) (*url.URL, error) {
		if !isCodexAccountOrigin(target) {
			return nil, errors.New("provider: unsupported Codex account-routing origin")
		}
		return proxyFunc(target)
	}, nil
}

func ValidateCodexAccountRoutingTarget(proxyURL, targetURL string) error {
	proxyFunc, err := CodexAccountRoutingProxyFunc(proxyURL)
	if err != nil {
		return err
	}
	target, err := url.Parse(targetURL)
	if err != nil || !isCodexAccountOrigin(target) {
		return errors.New("provider: unsupported Codex account-routing origin")
	}
	_, err = proxyFunc(target)
	return err
}

func isCodexAccountOrigin(target *url.URL) bool {
	return target != nil && target.Scheme == "https" && strings.EqualFold(target.Hostname(), "chatgpt.com") && target.User == nil && (target.Port() == "" || target.Port() == "443")
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
