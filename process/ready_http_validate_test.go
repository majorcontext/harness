package process

import (
	"strings"
	"testing"
)

// TestValidateDefReadyHTTPRejectsNonHTTP encodes the PR#73 review finding:
// url.ParseRequestURI accepts inputs http.Get can never satisfy — a
// forgotten scheme ("localhost:3000/health" parses with scheme
// "localhost"), a non-HTTP scheme (ftp://), an empty host (http:///p) —
// and the ready gate then spins silently until timeout, exactly the
// failure class ready_http exists to eliminate.
func TestValidateDefReadyHTTPRejectsNonHTTP(t *testing.T) {
	base := []string{"sh", "-c", "true"}
	bad := []string{
		"localhost:3000/health", // missing http:// — parses as scheme "localhost"
		"ftp://host/path",       // non-HTTP scheme
		"http:///path",          // empty host
		"host/health",           // no scheme at all
	}
	for _, u := range bad {
		err := ValidateDef(Def{Command: base, ReadyHTTP: u})
		if err == nil {
			t.Errorf("ValidateDef accepted ready_http %q, want rejection", u)
			continue
		}
		if !strings.Contains(err.Error(), "ready_http") {
			t.Errorf("ready_http %q: error %q does not name the field", u, err)
		}
	}
	for _, u := range []string{"http://localhost:3000/health", "https://example.test/up"} {
		if err := ValidateDef(Def{Command: base, ReadyHTTP: u}); err != nil {
			t.Errorf("ValidateDef rejected valid ready_http %q: %v", u, err)
		}
	}
}
