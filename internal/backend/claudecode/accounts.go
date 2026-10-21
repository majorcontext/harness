package claudecode

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/provider"
)

// anthropicAPI is the endpoint of the CLI when ANTHROPIC_BASE_URL is unset.
const anthropicAPI = "https://api.anthropic.com"

// RouteAccounts sets the routing of the accounts of the vendor of the entry: a turn whose session selects an account
// for that vendor runs the CLI behind the proxy of the account. nil routing, or a nil getenv, makes such a turn fail.
func (b *Backend) RouteAccounts(routing *config.AccountRouting, getenv func(string) string) {
	b.routing, b.getenv = routing, getenv
}

// accountEnv returns env with the proxy variables of account, in upper and lower case. It fails when the account has
// no routing, when the proxy URL is unusable, or when NO_PROXY of env lets a target of the CLI bypass the proxy.
func (b *Backend) accountEnv(env []string, account string) ([]string, error) {
	if b.routing == nil {
		return nil, provider.MarkPermanent(fmt.Errorf("claudecode: account %q of vendor %q has no routing: set account_routing in the config", account, b.p.Vendor))
	}
	u, err := b.routing.ProxyURL(b.getenv, account)
	if err != nil {
		return nil, provider.MarkPermanent(fmt.Errorf("claudecode: %w", err))
	}
	lookup := func(key string) string {
		for i := len(env) - 1; i >= 0; i-- {
			if v, ok := strings.CutPrefix(env[i], key+"="); ok {
				return v
			}
		}
		return ""
	}
	targets := []*url.URL{{Scheme: "https", Host: "api.anthropic.com"}}
	if base := lookup("ANTHROPIC_BASE_URL"); base != "" {
		t, err := url.Parse(base)
		if err != nil || t.Hostname() == "" {
			return nil, provider.MarkPermanent(fmt.Errorf("claudecode: account routing: ANTHROPIC_BASE_URL is not a URL with a host"))
		}
		targets = []*url.URL{t}
	}
	noProxy := lookup("NO_PROXY")
	if noProxy == "" {
		noProxy = lookup("no_proxy")
	}
	if err := config.NoProxyBypass(noProxy, targets...); err != nil {
		return nil, provider.MarkPermanent(fmt.Errorf("claudecode: %w", err))
	}
	s := u.String()
	return append(env, "HTTPS_PROXY="+s, "HTTP_PROXY="+s, "https_proxy="+s, "http_proxy="+s), nil
}
