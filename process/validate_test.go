package process

import (
	"strings"
	"testing"
)

func TestValidateDef(t *testing.T) {
	cases := []struct {
		name    string
		def     Def
		wantErr string // substring, empty means no error
	}{
		{
			name:    "empty command",
			def:     Def{},
			wantErr: "command is required",
		},
		{
			name: "valid, no gate",
			def:  Def{Command: []string{"x"}},
		},
		{
			name: "valid ports",
			def:  Def{Command: []string{"x"}, Ports: []int{3000, 8080}},
		},
		{
			name:    "port too low",
			def:     Def{Command: []string{"x"}, Ports: []int{0}},
			wantErr: "out of range",
		},
		{
			name:    "port too high",
			def:     Def{Command: []string{"x"}, Ports: []int{65536}},
			wantErr: "out of range",
		},
		{
			name: "valid ready_regex",
			def:  Def{Command: []string{"x"}, ReadyRegex: "Ready in .*ms"},
		},
		{
			name:    "invalid ready_regex",
			def:     Def{Command: []string{"x"}, ReadyRegex: "("},
			wantErr: "invalid ready_regex",
		},
		{
			name: "valid ready_port",
			def:  Def{Command: []string{"x"}, ReadyPort: 3000},
		},
		{
			name:    "ready_port out of range",
			def:     Def{Command: []string{"x"}, ReadyPort: 70000},
			wantErr: "invalid ready_port",
		},
		{
			name: "valid ready_http",
			def:  Def{Command: []string{"x"}, ReadyHTTP: "http://localhost:3000/healthz"},
		},
		{
			name:    "invalid ready_http",
			def:     Def{Command: []string{"x"}, ReadyHTTP: "::not a url::"},
			wantErr: "invalid ready_http",
		},
		{
			name:    "ready_regex and ready_port both set",
			def:     Def{Command: []string{"x"}, ReadyRegex: "ready", ReadyPort: 3000},
			wantErr: "at most one of ready_regex, ready_port, ready_http",
		},
		{
			name:    "ready_regex and ready_http both set",
			def:     Def{Command: []string{"x"}, ReadyRegex: "ready", ReadyHTTP: "http://localhost:3000"},
			wantErr: "at most one of ready_regex, ready_port, ready_http",
		},
		{
			name:    "ready_port and ready_http both set",
			def:     Def{Command: []string{"x"}, ReadyPort: 3000, ReadyHTTP: "http://localhost:3000"},
			wantErr: "at most one of ready_regex, ready_port, ready_http",
		},
		{
			name:    "all three set",
			def:     Def{Command: []string{"x"}, ReadyRegex: "ready", ReadyPort: 3000, ReadyHTTP: "http://localhost:3000"},
			wantErr: "at most one of ready_regex, ready_port, ready_http",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateDef(c.def)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateDef(%+v) = %v, want nil", c.def, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateDef(%+v) = nil, want error containing %q", c.def, c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("ValidateDef(%+v) error = %q, want it to contain %q", c.def, err.Error(), c.wantErr)
			}
		})
	}
}
