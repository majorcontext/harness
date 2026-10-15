// Package webfetch holds the web_fetch tool: a GET of a public http or https
// URL, returned as markdown, text, or the raw body.
package webfetch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // register the GIF decoder for image.DecodeConfig
	_ "image/jpeg" // register the JPEG decoder for image.DecodeConfig
	_ "image/png"  // register the PNG decoder for image.DecodeConfig
	"io"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	_ "golang.org/x/image/webp" // register the WebP decoder for image.DecodeConfig

	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// Name is the name of the tool.
const Name = turn.WebFetchTool

const (
	timeout       = 30 * time.Second
	maxBody       = 10 << 20
	maxRedirects  = 5
	defaultMax    = 200_000
	acceptHeader  = "text/markdown, text/html;q=0.9, text/plain;q=0.8, application/json;q=0.8, */*;q=0.1"
	schemaPattern = `{
	"type": "object",
	"properties": {
		"url": {"type": "string", "description": "The http or https URL to read."},
		"format": {"type": "string", "enum": ["markdown", "text", "raw"], "description": "How to return an HTML page: markdown (default, keeps links), text (plain text), or raw (the source). Other text types pass through."},
		"max_bytes": {"type": "integer", "minimum": 1, "description": "Most bytes of text to return. Default 200000. A longer page is cut and the result says so."}
	},
	"required": ["url"]
}`
	description = "Read a web page or a text file at an http or https URL with a GET request, and return its text. " +
		"Use it to read a page the user gives you or a page you found by search. Prefer it over curl in bash. " +
		"It reads public pages only: an address on a private or local network is refused. It sends no cookie and adds no credential. " +
		"An HTML page becomes markdown with its links (format markdown), plain text (format text), or its source (format raw). " +
		"Text and JSON pass through. A PNG, JPEG, GIF, or WebP image comes back as an image. " +
		"Any other type answers with its type and size. The result starts with the final URL and the status."
)

// Options configures the tool.
type Options struct {
	// Version is the build version that the User-Agent names. Empty: no version.
	Version string
	// AllowLoopback lets the tool reach 127.0.0.0/8 and ::1, for tests.
	AllowLoopback bool
	// Getenv reads HTTP_PROXY, HTTPS_PROXY, and NO_PROXY. nil: no proxy.
	Getenv func(key string) string
	// Resolver resolves host names. nil: the system resolver.
	Resolver interface {
		LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
	}
}

type tool struct {
	client *http.Client
	agent  string
}

// New returns the web_fetch tool.
func New(o Options) turn.Tool {
	agent := "harness"
	if o.Version != "" {
		agent += "/" + o.Version
	}
	return &tool{
		client: &http.Client{Transport: newTransport(o), CheckRedirect: checkRedirect},
		agent:  agent + " (web_fetch)",
	}
}

func (*tool) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: Name, Description: description, InputSchema: json.RawMessage(schemaPattern)}
}

// redirectError reports a redirect that the tool refuses.
type redirectError struct{ reason string }

func (e *redirectError) Error() string { return e.reason }

func checkRedirect(req *http.Request, via []*http.Request) error {
	switch {
	case len(via) > maxRedirects:
		return &redirectError{fmt.Sprintf("stopped after %d redirects", maxRedirects)}
	case req.URL.Scheme != "http" && req.URL.Scheme != "https":
		return &redirectError{fmt.Sprintf("refused: redirect to the scheme %q; only http and https are allowed", req.URL.Scheme)}
	case req.URL.User != nil:
		return &redirectError{"refused: redirect to a URL with a user name or password"}
	}
	req.Header.Del("Referer")
	return nil
}

type input struct {
	URL      string `json:"url"`
	Format   string `json:"format"`
	MaxBytes int    `json:"max_bytes"`
}

func parseInput(args json.RawMessage) (input, *url.URL, error) {
	in := input{MaxBytes: defaultMax}
	if err := json.Unmarshal(args, &in); err != nil || in.URL == "" {
		return in, nil, errors.New("web_fetch: missing url argument")
	}
	switch in.Format {
	case "":
		in.Format = "markdown"
	case "markdown", "text", "raw":
	default:
		return in, nil, fmt.Errorf("web_fetch: format %q is not one of markdown, text, raw", in.Format)
	}
	switch {
	case in.MaxBytes < 1 || in.MaxBytes > maxBody:
		return in, nil, fmt.Errorf("web_fetch: max_bytes must be between 1 and %d", maxBody)
	}
	u, err := url.Parse(in.URL)
	switch {
	case err != nil || u.Scheme == "":
		return in, nil, errors.New("web_fetch: url must be an absolute http or https URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return in, nil, fmt.Errorf("web_fetch: the scheme %q is refused; only http and https are allowed", u.Scheme)
	case u.Host == "":
		return in, nil, errors.New("web_fetch: url must be an absolute http or https URL")
	case u.User != nil:
		return in, nil, errors.New("web_fetch: refused: a URL with a user name or password")
	}
	return in, u, nil
}

func (t *tool) Run(ctx context.Context, call protocol.ToolCall) (protocol.ToolResult, error) {
	in, u, err := parseInput(call.Arguments)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return protocol.ToolResult{}, errors.New("web_fetch: url must be an absolute http or https URL")
	}
	req.Header.Set("User-Agent", t.agent)
	req.Header.Set("Accept", acceptHeader)
	resp, err := t.client.Do(req)
	if err != nil {
		return protocol.ToolResult{}, fetchError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return protocol.ToolResult{}, fetchError(ctx, err)
	}
	cut := len(data) > maxBody
	if cut {
		data = data[:maxBody]
	}
	res, err := render(ctx, resp, data, cut, in)
	if err != nil {
		return protocol.ToolResult{}, fetchError(ctx, err)
	}
	return res, nil
}

// fetchError turns the error of a request into the text that the model reads.
func fetchError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("web_fetch: timeout after %s", timeout)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("web_fetch: aborted: %w", ctx.Err())
	}
	if np, ok := errors.AsType[*notPublicError](err); ok {
		return fmt.Errorf("web_fetch: %w", np)
	}
	if re, ok := errors.AsType[*redirectError](err); ok {
		return fmt.Errorf("web_fetch: %w", re)
	}
	if ue, ok := errors.AsType[*url.Error](err); ok {
		err = ue.Err
	}
	return fmt.Errorf("web_fetch: request failed: %w", err)
}

var textTypes = map[string]bool{
	"application/json": true, "application/xml": true, "application/javascript": true, "application/x-javascript": true,
	"application/ecmascript": true, "application/yaml": true, "application/x-yaml": true, "application/toml": true,
	"application/xhtml+xml": true, "application/x-ndjson": true,
}

func isText(mt string) bool {
	return strings.HasPrefix(mt, "text/") || textTypes[mt] || strings.HasSuffix(mt, "+json") || strings.HasSuffix(mt, "+xml")
}

// imageTypes maps the image media types to the format name that image.DecodeConfig reports.
var imageTypes = map[string]string{"image/png": "png", "image/jpeg": "jpeg", "image/gif": "gif", "image/webp": "webp"}

// mediaType returns the media type of the response, or the sniffed type of its body.
func mediaType(h http.Header, data []byte) string {
	mt, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil || mt == "" {
		mt, _, _ = mime.ParseMediaType(http.DetectContentType(data))
	}
	return mt
}

// render builds the result of one response. cut says that the body is above
// the body limit and data holds its first bytes.
func render(ctx context.Context, resp *http.Response, data []byte, cut bool, in input) (protocol.ToolResult, error) {
	mt := mediaType(resp.Header, data)
	var head strings.Builder
	fmt.Fprintf(&head, "URL: %s\nStatus: %s\n", resp.Request.URL, resp.Status)
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		fmt.Fprintf(&head, "Content-Type: %s\n", ct)
	}
	res := protocol.ToolResult{IsError: resp.StatusCode >= 400}
	var body string
	htmlCut := false
	switch {
	case !cut && sniffsAsImage(data, mt):
		cfg, _, _ := image.DecodeConfig(bytes.NewReader(data))
		body = fmt.Sprintf("image (%s), %d bytes, %dx%d pixels", mt, len(data), cfg.Width, cfg.Height)
		res.Blobs = []protocol.Blob{{MediaType: mt, Data: data}}
	case isText(mt):
		tb, err := textOf(ctx, &head, resp, data, mt, in)
		if err != nil {
			return res, err
		}
		body, htmlCut = limit(tb.s, in.MaxBytes, tb.extra), tb.htmlCut
	default:
		size := fmt.Sprintf("%d bytes", len(data))
		if cut {
			size = fmt.Sprintf("at least %d bytes", len(data))
		}
		body = fmt.Sprintf("binary content (%s), %s: not shown", mt, size)
	}
	if htmlCut {
		body += fmt.Sprintf("\n\n[only the first %d bytes of the HTML were converted]", maxParse)
	}
	if cut && isText(mt) {
		body += fmt.Sprintf("\n\n[the body is above the limit of %d bytes; the rest was not read]", maxBody)
	}
	res.Text = head.String() + "\n" + body
	return res, nil
}

// sniffsAsImage reports whether data decodes as the image format that mt declares.
func sniffsAsImage(data []byte, mt string) bool {
	_, format, err := image.DecodeConfig(bytes.NewReader(data))
	f, ok := imageTypes[mt]
	return err == nil && ok && f == format
}

// textBody is a text body. extra counts the bytes that follow it and that the
// tool did not copy; -1 says that more follows, of an unknown size. htmlCut says
// that only the first maxParse bytes of an HTML body were parsed.
type textBody struct {
	s       string
	extra   int
	htmlCut bool
}

// maxParse bounds the HTML that the tool parses. The parse costs memory and
// time before the converter can stop, and the rest is beyond any max_bytes
// that is useful.
const maxParse = 2 << 20

// textOf returns a text body, and adds the title of an HTML page to head.
func textOf(ctx context.Context, head *strings.Builder, resp *http.Response, data []byte, mt string, in input) (textBody, error) {
	if err := ctx.Err(); err != nil {
		return textBody{}, err
	}
	if (mt == "text/html" || mt == "application/xhtml+xml") && in.Format != "raw" {
		doc := data[:min(len(data), maxParse)]
		pg, err := convertHTML(ctx, bytes.NewReader(doc), resp.Request.URL, in.Format == "markdown", in.MaxBytes)
		if ctx.Err() != nil {
			return textBody{}, ctx.Err()
		}
		if err == nil {
			if pg.Title != "" {
				fmt.Fprintf(head, "Title: %s\n", pg.Title)
			}
			extra := 0
			if pg.Truncated {
				extra = -1
			}
			return textBody{s: pg.Body, extra: extra, htmlCut: len(doc) < len(data)}, nil
		}
	}
	keep := data[:min(len(data), in.MaxBytes+utf8.UTFMax)]
	return textBody{s: strings.ToValidUTF8(string(keep), "\uFFFD"), extra: len(data) - len(keep)}, nil
}

// limit cuts s at n bytes, at a rune boundary, and says how much it cut. extra
// is the count of bytes that follow s; -1 says that more follows, of an unknown size.
func limit(s string, n, extra int) string {
	if len(s) <= n && extra == 0 {
		return s
	}
	end := min(n, len(s))
	for end > 0 && end < len(s) && !utf8.RuneStart(s[end]) {
		end--
	}
	if extra < 0 {
		return fmt.Sprintf("%s\n\n[cut at max_bytes=%d; the page has more content. Call web_fetch again with a larger max_bytes to read more.]", s[:end], n)
	}
	return fmt.Sprintf("%s\n\n[cut at max_bytes=%d; %d more bytes. Call web_fetch again with a larger max_bytes to read more.]", s[:end], n, len(s)-end+extra)
}
