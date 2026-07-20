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
