package engine

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
)

func proxyEnvironment(env []string, proxyURL string) []string {
	names := map[string]bool{"HTTP_PROXY": true, "http_proxy": true, "HTTPS_PROXY": true, "https_proxy": true}
	out := make([]string, 0, len(env)+4)
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || !names[name] {
			out = append(out, entry)
		}
	}
	return append(out, "HTTP_PROXY="+proxyURL, "http_proxy="+proxyURL, "HTTPS_PROXY="+proxyURL, "https_proxy="+proxyURL)
}

func ValidateAccountProxyURL(raw string) error {
	_, _, _, err := parseAccountProxyURL(raw)
	return err
}

func accountProxyURL(raw string, selection map[string]string) (string, error) {
	base, username, password, err := parseAccountProxyURL(raw)
	if err != nil {
		return "", err
	}
	if len(selection) == 0 {
		return "", nil
	}
	for vendor, id := range selection {
		if vendor != "claude" && vendor != "codex" || !validSubscriptionAccountID(id) {
			return "", errors.New("invalid subscription account selection")
		}
	}
	payload, err := json.Marshal(selection)
	if err != nil {
		return "", errors.New("invalid subscription account selection")
	}
	if len(payload) > 512 {
		return "", errors.New("subscription account selection exceeds 512 bytes")
	}
	parts := strings.Split(username, "|")
	identity := strings.Join(parts[:2], "|")
	username = identity + "|accounts-v1=" + base64.RawURLEncoding.EncodeToString(payload)
	base.User = url.UserPassword(username, password)
	return base.String(), nil
}

func parseAccountProxyURL(raw string) (*url.URL, string, string, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User == nil {
		return nil, "", "", errors.New("invalid account-routing proxy URL")
	}
	username := u.User.Username()
	password, hasPassword := u.User.Password()
	if !hasPassword || password == "" || strings.Contains(username, ":") {
		return nil, "", "", errors.New("invalid account-routing proxy credentials")
	}
	parts := strings.Split(username, "|")
	if len(parts) == 3 && strings.HasPrefix(parts[2], "accounts-v1=") {
		encoded := strings.TrimPrefix(parts[2], "accounts-v1=")
		data, err := base64.RawURLEncoding.DecodeString(encoded)
		var selection map[string]string
		if err != nil || len(data) > 512 || json.Unmarshal(data, &selection) != nil || selection == nil {
			return nil, "", "", errors.New("invalid account-routing proxy selector")
		}
		parts = parts[:2]
		username = strings.Join(parts, "|")
	} else if len(parts) != 2 {
		return nil, "", "", errors.New("invalid account-routing proxy username")
	}
	if parts[0] == "" || parts[1] == "" || strings.Contains(parts[0], ":") || strings.Contains(parts[1], ":") {
		return nil, "", "", errors.New("invalid account-routing proxy identity")
	}
	return u, username, password, nil
}
