package toolresult_test

import (
	"testing"

	"github.com/majorcontext/harness/internal/toolresult"
)

func TestMask(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"AWS_SECRET_ACCESS_KEY=abcdefgh123", "AWS_SECRET_ACCESS_KEY=***"},
		{"password: hunter2hunter2\nok", "password: ***\nok"},
		{`{"api_key": "sk-123"}`, `{"api_key": "***"}`},
		{"Authorization: Bearer abcdefgh.ijk", "Authorization: Bearer ***"},
		{`export TOKEN="secret value"`, `export TOKEN="***"`},
		{"TOKEN='a b c'", "TOKEN='***'"},
		{"token:=lexer.Next()", "token:=lexer.Next()"},
		{"https://x/?a=1&token=abcdefgh1234&b=2", "https://x/?a=1&token=***&b=2"},
		{"token: short", "token: short"},
		{"no keys here", "no keys here"},
	} {
		if got := toolresult.Mask(tc.in); got != tc.want {
			t.Errorf("Mask(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
