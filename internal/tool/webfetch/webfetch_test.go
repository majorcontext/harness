package webfetch

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPublicClassifiesAddresses(t *testing.T) {
	for _, tc := range []struct {
		addr     string
		loopback bool
		want     bool
	}{
		{"93.184.216.34", false, true},
		{"8.8.8.8", false, true},
		{"2606:4700:4700::1111", false, true},
		{"::ffff:8.8.8.8", false, true},
		{"64:ff9b::808:808", false, true},
		{"127.0.0.1", false, false},
		{"127.0.0.1", true, true},
		{"::1", false, false},
		{"::1", true, true},
		{"::ffff:127.0.0.1", false, false},
		{"::ffff:127.0.0.1", true, true},
		{"10.0.0.1", true, false},
		{"172.16.5.4", false, false},
		{"172.32.0.1", false, true},
		{"192.168.0.1", false, false},
		{"100.64.0.1", false, false},
		{"100.127.255.255", false, false},
		{"100.128.0.1", false, true},
		{"169.254.169.254", false, false},
		{"fe80::1", false, false},
		{"fe80::1%eth0", false, false},
		{"fc00::1", false, false},
		{"fd12:3456::1", false, false},
		{"224.0.0.1", false, false},
		{"ff02::1", false, false},
		{"0.0.0.0", false, false},
		{"0.1.2.3", false, false},
		{"::", false, false},
		{"::ffff:10.0.0.1", false, false},
		{"::ffff:192.168.1.1", false, false},
		{"::ffff:169.254.169.254", false, false},
		{"64:ff9b::a00:1", false, false},
		{"2002:a00:1::1", false, false},
		{"2002:808:808::1", false, true},
		{"2001:0:4136:e378:8000:63bf:3fff:fdd2", false, false},
		{"255.255.255.255", false, false},
		{"198.18.0.1", false, false},
		{"64:ff9b:1::a00:1", false, false},
		{"::7f00:1", false, false},
		{"::a00:1", false, false},
		{"::808:808", false, false},
		{"fec0::1", false, false},
		{"feff::1", false, false},
	} {
		addr := netip.MustParseAddr(tc.addr)
		if got := public(addr, tc.loopback); got != tc.want {
			t.Errorf("public(%s, loopback=%v) = %v, want %v", tc.addr, tc.loopback, got, tc.want)
		}
	}
	if public(netip.Addr{}, true) {
		t.Error("the zero address is public")
	}
}

type fakeResolver map[string][]netip.Addr

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := f[host]; ok {
		return a, nil
	}
	return nil, errors.New("no such host")
}

func TestGuardedDialChecksEveryResolvedAddressAndDialsTheChecked(t *testing.T) {
	var dialed []string
	dial := func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		return nil, errors.New("dial stopped")
	}
	res := fakeResolver{
		"good.example":  {netip.MustParseAddr("93.184.216.34")},
		"mixed.example": {netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("10.0.0.5")},
		"local.example": {netip.MustParseAddr("127.0.0.1")},
		"empty.example": {},
	}
	g := guardedDial(dial, res, false)
	for _, tc := range []struct {
		addr       string
		wantDialed string
		wantRefuse bool
	}{
		{"good.example:443", "93.184.216.34:443", false},
		{"mixed.example:80", "", true},
		{"local.example:80", "", true},
		{"10.1.2.3:80", "", true},
		{"[::ffff:10.1.2.3]:80", "", true},
		{"8.8.8.8:53", "8.8.8.8:53", false},
	} {
		dialed = nil
		_, err := g(t.Context(), "tcp", tc.addr)
		if _, refused := errors.AsType[*notPublicError](err); refused != tc.wantRefuse {
			t.Errorf("%s: err = %v, want refusal %v", tc.addr, err, tc.wantRefuse)
		}
		if got := strings.Join(dialed, ","); got != tc.wantDialed {
			t.Errorf("%s: dialed %q, want %q", tc.addr, got, tc.wantDialed)
		}
	}
	if _, err := g(t.Context(), "tcp", "empty.example:80"); err == nil {
		t.Error("a host with no address dialed")
	}
	if _, err := g(t.Context(), "tcp", "unknown.example:80"); err == nil {
		t.Error("an unresolved host dialed")
	}
	_, err := guardedDial(dial, res, true)(t.Context(), "tcp", "local.example:80")
	if _, refused := errors.AsType[*notPublicError](err); refused || err == nil {
		t.Errorf("with loopback allowed: err = %v, want the error of the dial", err)
	}
}

func TestConvertHTML(t *testing.T) {
	base, _ := url.Parse("https://site.example/dir/page")
	for _, tc := range []struct {
		name, html string
		markdown   bool
		want       string
	}{
		{"headings and paragraphs", `<h1>One</h1><h3>Three</h3><p>Hello   <b>big</b>
		world.</p>`, true, "# One\n\n### Three\n\nHello **big** world."},
		{"relative links resolve", `<p><a href="../up">up</a> <a href="/root">root</a> <a href="https://o.example/x">abs</a></p>`, true,
			"[up](https://site.example/up) [root](https://site.example/root) [abs](https://o.example/x)"},
		{"links that go nowhere keep the text", `<p><a href="javascript:x()">js</a> <a href="#top">top</a> <a href="mailto:a@b">mail</a></p>`, true, "js top mail"},
		{"script style nav footer and hidden are dropped", `<nav>menu</nav><script>evil()</script><style>x{}</style><p hidden>no</p><p aria-hidden="true">no</p><footer>foot</footer><p>kept</p>`, true, "kept"},
		{"lists nest", `<ul><li>a<ul><li>b</li></ul></li></ul><ol start="3"><li>c</li><li>d</li></ol>`, true, "- a\n  - b\n\n3. c\n4. d"},
		{"code blocks fence", `<pre><code class="language-sh">ls -l
</code></pre><p>use <code>x</code></p>`, true, "```sh\nls -l\n```\n\nuse `x`"},
		{"blockquote", `<blockquote><p>quoted</p><p>more</p></blockquote>`, true, "> quoted\n>\n> more"},
		{"table", `<table><tr><th>A</th><th>B</th></tr><tr><td>1</td><td>a|b</td></tr></table>`, true, "| A | B |\n| --- | --- |\n| 1 | a\\|b |"},
		{"text mode drops marks and link targets", `<h2>Head</h2><p><a href="/x">link</a> <em>em</em> <code>c</code></p><hr>`, false, "Head\n\nlink em c"},
		{"image alt", `<p><img src="/i.png" alt="logo"><img src="/j.png"></p>`, true, "![logo](https://site.example/i.png)"},
		{"line break", `<p>a<br>b</p>`, true, "a\nb"},
		{"a block inside a link is one run", `<a href="/c"><div>Card</div><div>title</div></a>`, true, "[Card title](https://site.example/c)"},
		{"invalid UTF-8", "<p>a\xffb</p>", true, "a�b"},
		{"unclosed tags", `<p>one<p>two<ul><li>x`, true, "one\n\ntwo\n\n- x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pg, err := convertHTML(t.Context(), strings.NewReader(tc.html), base, tc.markdown, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			if pg.Body != tc.want {
				t.Errorf("body = %q, want %q", pg.Body, tc.want)
			}
		})
	}
	pg, _ := convertHTML(t.Context(), strings.NewReader("<title> A  title </title><p>x</p>"), base, true, 1<<20)
	if pg.Title != "A title" {
		t.Errorf("title = %q", pg.Title)
	}
}

func TestLimitCutsAtARuneBoundary(t *testing.T) {
	got := limit("aéb", 2, 0)
	if !strings.HasPrefix(got, "a\n\n[cut at max_bytes=2; 3 more bytes.") {
		t.Errorf("limit = %q", got)
	}
	if limit("abc", 3, 0) != "abc" {
		t.Error("a text at the limit was cut")
	}
}

func TestCheckRedirect(t *testing.T) {
	req := func(raw string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Referer", "https://before.example/")
		return r
	}
	for _, tc := range []struct {
		name, to string
		hops     int
		wantErr  string
	}{
		{"https hop", "https://a.example/x", 1, ""},
		{"fifth redirect", "https://a.example/x", 5, ""},
		{"sixth redirect", "https://a.example/x", 6, "stopped after 5 redirects"},
		{"file scheme", "file:///etc/passwd", 1, "scheme"},
		{"ftp scheme", "ftp://a.example/x", 1, "scheme"},
		{"credentials", "https://u:p@a.example/", 1, "user name"},
	} {
		r := req(tc.to)
		err := checkRedirect(r, make([]*http.Request, tc.hops))
		switch {
		case tc.wantErr == "" && err != nil, tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.wantErr)
		case err == nil && r.Header.Get("Referer") != "":
			t.Errorf("%s: the redirect keeps its Referer", tc.name)
		}
	}
}

func TestConvertHTMLBoundsHostileNesting(t *testing.T) {
	base, _ := url.Parse("https://site.example/")
	const max = 100_000
	pages := map[string]string{
		"blockquote": strings.Repeat("<blockquote>", 500) + strings.Repeat("line of text<br>", 5000) + strings.Repeat("</blockquote>", 500),
		"list":       strings.Repeat("<ul><li>item ", 250) + strings.Repeat("</li></ul>", 250),
		"inline":     strings.Repeat("<b>", 500) + "text" + strings.Repeat("</b>", 500),
		"div":        strings.Repeat("<div>", 500) + "deep text" + strings.Repeat("</div>", 500),
		"table":      strings.Repeat("<table><tr><td>", 100) + "cell" + strings.Repeat("</td></tr></table>", 100),
	}
	for name, doc := range pages {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			pg, err := convertHTML(t.Context(), strings.NewReader(doc), base, true, max)
			if err != nil {
				t.Fatal(err)
			}
			if len(pg.Body) > 2*max {
				t.Errorf("body is %d bytes, want at most %d", len(pg.Body), 2*max)
			}
			if d := time.Since(start); d > 10*time.Second {
				t.Errorf("conversion took %s", d)
			}
		})
	}
}

func TestConvertHTMLStopsAtTheByteBound(t *testing.T) {
	doc := strings.Repeat("<p>paragraph of some text</p>", 10_000)
	pg, err := convertHTML(t.Context(), strings.NewReader(doc), nil, true, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !pg.Truncated || len(pg.Body) > 1100 {
		t.Errorf("truncated = %v, body = %d bytes, want a stop near 1000", pg.Truncated, len(pg.Body))
	}
}

func TestConvertHTMLStopsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := convertHTML(ctx, strings.NewReader("<p>x</p>"), nil, true, 1000); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestNestedQuoteAndListKeepTheirPrefixes(t *testing.T) {
	pg, err := convertHTML(t.Context(), strings.NewReader(`<blockquote><p>a</p><blockquote><p>b</p></blockquote></blockquote><ul><li><ul><li>x</li></ul></li></ul>`), nil, true, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if want := "> a\n>\n> > b\n\n- - x"; pg.Body != want {
		t.Errorf("body = %q, want %q", pg.Body, want)
	}
}
