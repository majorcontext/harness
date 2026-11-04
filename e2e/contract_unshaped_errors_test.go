package e2e

import (
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

var unshapedErrorRows = map[string]runtimeRow{
	"unshaped_provider_error_plain":      sameAsServe(),
	"unshaped_provider_error_html":       sameAsServe(),
	"unshaped_provider_error_long":       sameAsServe(),
	"unshaped_provider_error_boundary":   sameAsServe(),
	"unshaped_provider_error_whitespace": sameAsServe(),
	"unshaped_provider_error_empty":      sameAsServe(),
	"unshaped_provider_error_controls":   sameAsServe(),
	"unshaped_provider_error_json":       sameAsServe(),
	"unshaped_provider_error_anthropic":  sameAsServe(),
	"unshaped_provider_error_compat":     sameAsServe(),
}

type expectTurnError struct{ want string }

func (a expectTurnError) run(t *testing.T, r *run) {
	t.Helper()
	page := r.drv.JournalPage(t, r.id(t, "a"), 0, 1000)
	body := page.Body.(map[string]any)
	for _, raw := range body["events"].([]any) {
		event := raw.(map[string]any)
		if event["k"] == "turn.ended" {
			data := event["d"].(map[string]any)
			if data["error"] != a.want {
				t.Fatalf("turn error = %q, want %q", data["error"], a.want)
			}
			r.record(t, "turn_error", "a", callResult{Status: page.Status, Body: data["error"]})
			return
		}
	}
	t.Fatal("no turn.ended event")
}

func TestContractUnshapedProviderErrors(t *testing.T) {
	t.Parallel()
	rows := []struct{ name, body, want string }{
		{"plain", "request is too large\ntry less", "request is too large try less"},
		{"html", "<html>\n <body>bad request</body>\n</html>", "<html> <body>bad request</body> </html>"},
		{"long", strings.Repeat(" ", 511) + "A" + strings.Repeat("B", 88), "A ...[truncated]"},
		{"boundary", strings.Repeat(" ", 511) + "A", "A"},
		{"whitespace", strings.Repeat(" ", 513), "...[truncated]"},
		{"empty", "", ""},
		{"controls", "bad\x00\tresponse\xff", "bad response�"},
		{"json", `{"detail":"refused"}`, `{"detail":"refused"}`},
	}
	var scenarios []codexScenario
	for _, row := range rows {
		want := "openai: HTTP 400"
		if row.want != "" {
			want += ": " + row.want
		}
		scenarios = append(scenarios, codexScenario{scenario: scenario{
			name:    "unshaped_provider_error_" + row.name,
			model:   []harnesstest.Step{{Name: "error", Reply: harnesstest.Reply{HTTPStatus: 400, ErrorBody: &row.body}}},
			actions: []action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}, expectTurnError{want: want}},
		}})
	}
	runCodexScenarios(t, scenarios)
}

func TestContractOtherUnshapedProviderErrors(t *testing.T) {
	body := "bad\x00  request\nrefused"
	rows := []struct {
		name, prefix string
		chat         bool
	}{
		{"anthropic", "anthropic", false},
		{"compat", "openaicompat(bifrost)", true},
	}
	var scenarios []scenario
	for _, row := range rows {
		scenarios = append(scenarios, scenario{
			name: "unshaped_provider_error_" + row.name, chat: row.chat,
			model:   []harnesstest.Step{{Name: "error", Reply: harnesstest.Reply{HTTPStatus: 400, ErrorBody: &body}}},
			actions: []action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}, expectTurnError{want: row.prefix + ": HTTP 400: bad request refused"}},
		})
	}
	runScenarios(t, scenarios)
}
