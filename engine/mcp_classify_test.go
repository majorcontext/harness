package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/majorcontext/harness/mcp"
)

// TestClassifyMCPConnectError is a table test for classifyMCPConnectError:
// every raw connect/initialize error the mcp package can produce must map
// to one of a small, fixed set of SHORT, URL-free reasons — see
// classifyMCPConnectError's doc comment for why (Go's *url.Error stringifies
// as `Post "<full-URL>": cause`, and real MCP endpoints sometimes carry
// secrets in the URL path/query).
func TestClassifyMCPConnectError(t *testing.T) {
	secretURLErr := &url.Error{
		Op:  "Post",
		URL: "https://mcp.example.com/v1?token=SUPERSECRET123",
		Err: &net.OpError{
			Op:  "dial",
			Net: "tcp",
			Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED},
		},
	}

	dialFailedErr := &url.Error{
		Op:  "Post",
		URL: "https://mcp.example.com/v1?token=OTHERSECRET456",
		Err: &net.OpError{
			Op:  "dial",
			Net: "tcp",
			Err: errors.New("no route to host"),
		},
	}

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "initializing"},
		{"deadline exceeded directly", context.DeadlineExceeded, "initialize timed out"},
		{"deadline exceeded wrapped", fmt.Errorf("initialize: %w", context.DeadlineExceeded), "initialize timed out"},
		{"connection refused via url.Error/net.OpError", secretURLErr, "connection refused"},
		{"other dial failure via url.Error/net.OpError", dialFailedErr, "connection failed"},
		{"generic error", errors.New("boom"), "initialize failed"},
		{"http status error text", errors.New(`mcp: http 401: {"error":"unauthorized"}`), "initialize failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyMCPConnectError(tt.err)
			if got != tt.want {
				t.Errorf("classifyMCPConnectError(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

// TestClassifyMCPConnectErrorNeverLeaksURL is the no-secret-leak assertion
// called out explicitly in review: a crafted *url.Error whose URL carries a
// fake secret must never have that secret survive classification, for
// every classified branch a URL-bearing error can take.
func TestClassifyMCPConnectErrorNeverLeaksURL(t *testing.T) {
	const secret = "SUPERSECRET123"

	errs := []error{
		&url.Error{Op: "Post", URL: "https://mcp.example.com/v1?token=" + secret, Err: &net.OpError{
			Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED},
		}},
		&url.Error{Op: "Post", URL: "https://mcp.example.com/v1?token=" + secret, Err: &net.OpError{
			Op: "dial", Net: "tcp", Err: errors.New("no route to host"),
		}},
		fmt.Errorf("initialize: %w", fmt.Errorf("mcp: http request: %w", &url.Error{
			Op: "Post", URL: "https://mcp.example.com/v1?token=" + secret, Err: context.DeadlineExceeded,
		})),
	}

	// Sanity check: prove the raw error DOES contain the secret, so this
	// test is actually exercising the leak-prevention path and not
	// vacuously passing.
	for i, err := range errs {
		if !strings.Contains(err.Error(), secret) {
			t.Fatalf("errs[%d].Error() = %q, want it to contain the fake secret (test setup problem)", i, err.Error())
		}
	}

	for i, err := range errs {
		got := classifyMCPConnectError(err)
		if strings.Contains(got, secret) {
			t.Errorf("classifyMCPConnectError(errs[%d]) = %q, leaked the fake secret", i, got)
		}
		if strings.Contains(got, "mcp.example.com") {
			t.Errorf("classifyMCPConnectError(errs[%d]) = %q, leaked the endpoint host", i, got)
		}
	}
}

// TestSanitizeMCPCallError is a table test for sanitizeMCPCallError: the
// runtime-call sibling of classifyMCPConnectError, guarding the SAME
// secret-in-URL class (a connected server dropping mid-call yields a
// *url.Error stringifying as `Post "<full-endpoint-URL>": <cause>`, same as
// a failed connect) but on client.CallTool's error path instead of
// Initialize's. Only transport-shaped errors (*url.Error, *net.OpError) get
// sanitized; everything else — a server-sent *mcp.RPCError, a generic
// error, a bare context.Canceled/DeadlineExceeded (both HTTP and stdio
// transports return ctx.Err() bare, never wrapped in a *url.Error — see
// mcp/http.go's call() and mcp/conn.go's call()) — passes through
// unchanged, since none of those shapes carry the endpoint URL and the
// model needs the original text (e.g. an RPCError's message) to
// self-correct.
func TestSanitizeMCPCallError(t *testing.T) {
	secretURLErr := &url.Error{
		Op:  "Post",
		URL: "https://mcp.example.com/v1?token=SUPERSECRET123",
		Err: &net.OpError{
			Op:  "dial",
			Net: "tcp",
			Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED},
		},
	}
	bareOpErr := &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: errors.New("no route to host"),
	}
	rpcErr := &mcp.RPCError{Code: -32602, Message: "invalid arguments: missing \"city\""}
	genericErr := errors.New("boom")
	secretDeadlineErr := &url.Error{
		Op:  "Post",
		URL: "http://127.0.0.1:1/mcp?token=LEAKME",
		Err: context.DeadlineExceeded,
	}

	tests := []struct {
		name       string
		err        error
		wantExact  string // exact resulting error text, if non-empty
		wantSame   bool   // result must be the exact same error value (passthrough)
		wantNoText []string
	}{
		{
			name:       "url.Error wrapping connection-refused net.OpError",
			err:        secretURLErr,
			wantExact:  `engine: mcp: server "weather": call failed: connection refused`,
			wantNoText: []string{"SUPERSECRET123", "mcp.example.com"},
		},
		{
			name:      "bare net.OpError, other dial failure",
			err:       bareOpErr,
			wantExact: `engine: mcp: server "weather": call failed: connection failed`,
		},
		{
			name:     "server-sent RPCError passes through unchanged",
			err:      rpcErr,
			wantSame: true,
		},
		{
			name:     "generic non-transport error passes through unchanged",
			err:      genericErr,
			wantSame: true,
		},
		{
			name:     "bare context.Canceled passes through unchanged",
			err:      context.Canceled,
			wantSame: true,
		},
		{
			name:     "bare context.DeadlineExceeded passes through unchanged",
			err:      context.DeadlineExceeded,
			wantSame: true,
		},
		{
			// Pins the "call timed out" branch (errors.Is(err,
			// context.DeadlineExceeded) inside the transport-shaped gate):
			// unlike the bare context.DeadlineExceeded case above, this one
			// IS wrapped in a *url.Error (as a hung server long past
			// ConnectTimeout/RequestTimeout could plausibly produce, or a
			// defensive future transport change), so it must still be
			// classified rather than passed through raw with the URL.
			name:       "url.Error wrapping context.DeadlineExceeded",
			err:        secretDeadlineErr,
			wantExact:  `engine: mcp: server "weather": call timed out`,
			wantNoText: []string{"LEAKME", "127.0.0.1:1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeMCPCallError("weather", tt.err)
			if tt.wantSame {
				if got != tt.err {
					t.Errorf("sanitizeMCPCallError() = %v, want the exact same passthrough error %v", got, tt.err)
				}
				return
			}
			if got == nil {
				t.Fatal("sanitizeMCPCallError() = nil, want an error")
			}
			if tt.wantExact != "" && got.Error() != tt.wantExact {
				t.Errorf("sanitizeMCPCallError() = %q, want %q", got.Error(), tt.wantExact)
			}
			for _, s := range tt.wantNoText {
				if strings.Contains(got.Error(), s) {
					t.Errorf("sanitizeMCPCallError() = %q, leaked %q", got.Error(), s)
				}
			}
		})
	}
}
