package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

func startServeFlags(t *testing.T, workdir string, extra map[string]any, env map[string]string, args []string, steps ...harnesstest.Step) (*runtimeDriver, *harnesstest.Server) {
	t.Helper()
	fake := harnesstest.New(t, steps...)
	cfg := writeGoalConfigWith(t, fake.URL(), scenarioConfig(extra))
	return serveHost.openIn(t, cfg, workdir, env, args...).(*runtimeDriver), fake
}

func serveRequest(t *testing.T, p *serveProc, method, path, auth, origin string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, "http://"+p.addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v\nserve stderr:\n%s", method, path, err, p.stderr.String())
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestContractServeBearer(t *testing.T) {
	skipShort(t)
	p := startServe(t, t.TempDir(), writeGoalConfig(t, "http://127.0.0.1:1"))
	rows := []struct {
		name, path, auth string
		want             int
	}{
		{"bearer_missing_token_is_401", "/sessions", "", http.StatusUnauthorized},
		{"bearer_wrong_token_is_401", "/sessions", "Bearer wrong", http.StatusUnauthorized},
		{"bearer_right_token_is_served", "/sessions", "Bearer " + p.token, http.StatusOK},
		{"bearer_health_needs_no_token", "/health", "", http.StatusOK},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			resp := serveRequest(t, p, http.MethodGet, row.path, row.auth, "")
			if resp.StatusCode != row.want {
				t.Errorf("GET %s with %q = %d, want %d", row.path, row.auth, resp.StatusCode, row.want)
			}
			if row.want != http.StatusUnauthorized {
				return
			}
			var body protocol.ErrorBody
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Error.Code != "unauthorized" {
				t.Errorf("GET %s with %q body = %+v (%v), want the error code unauthorized", row.path, row.auth, body, err)
			}
		})
	}
}

func TestContractServeCORS(t *testing.T) {
	skipShort(t)
	const origin = "https://console.example"
	start := func(t *testing.T, args ...string) *serveProc {
		return startServeProc(t, freeAddr, t.TempDir(), map[string]string{
			"HARNESS_SESSION_DIR": t.TempDir(),
			"HARNESS_CONFIG":      writeGoalConfig(t, "http://127.0.0.1:1"),
			"ANTHROPIC_API_KEY":   "e2e-dummy-key",
		}, args...)
	}
	t.Run("cors_origin_answers_a_preflight_without_a_token", func(t *testing.T) {
		resp := serveRequest(t, start(t, "-cors-origin", origin), http.MethodOptions, "/sessions/ses:a/inputs", "", origin)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("preflight = %d, want 204", resp.StatusCode)
		}
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != origin {
			t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, origin)
		}
		for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
			if got := resp.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(got, method) {
				t.Errorf("Access-Control-Allow-Methods = %q, want %s", got, method)
			}
		}
		if got := resp.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Authorization") || !strings.Contains(got, "Last-Event-ID") {
			t.Errorf("Access-Control-Allow-Headers = %q, want Authorization and Last-Event-ID", got)
		}
	})
	t.Run("cors_origin_marks_every_response", func(t *testing.T) {
		p := start(t, "-cors-origin", origin)
		for _, auth := range []string{"", "Bearer " + p.token} {
			if got := serveRequest(t, p, http.MethodGet, "/sessions", auth, origin).Header.Get("Access-Control-Allow-Origin"); got != origin {
				t.Errorf("Access-Control-Allow-Origin with auth %q = %q, want %q", auth, got, origin)
			}
		}
	})
	t.Run("no_cors_origin_adds_no_header", func(t *testing.T) {
		p := start(t)
		resp := serveRequest(t, p, http.MethodGet, "/sessions", "Bearer "+p.token, origin)
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want none", got)
		}
	})
}

func TestContractServePromptFlags(t *testing.T) {
	skipShort(t)
	rows := []struct {
		name  string
		files map[string]string
		cfg   map[string]any
		env   map[string]string
		args  []string
		want  func(t *testing.T, system string)
	}{
		{
			name:  "no_instructions_flag_injects_nothing",
			files: map[string]string{"AGENTS.md": "# Rules\nBe brief.\n"},
			args:  []string{"-no-instructions"},
			want: func(t *testing.T, system string) {
				if strings.Contains(system, "Project instructions from") {
					t.Errorf("system prompt holds instructions: %q", system)
				}
			},
		},
		{
			name:  "no_instructions_flag_wins_over_the_environment",
			files: map[string]string{"AGENTS.md": "# Rules\nBe brief.\n"},
			env:   map[string]string{"HARNESS_INSTRUCTIONS": "true"},
			args:  []string{"-no-instructions"},
			want: func(t *testing.T, system string) {
				if strings.Contains(system, "Project instructions from") {
					t.Errorf("system prompt holds instructions: %q", system)
				}
			},
		},
		{
			name: "skills_dir_flag_replaces_the_config_dirs",
			files: map[string]string{
				"from-config/cfg/SKILL.md":  skillFile("cfg", "From config.", "x"),
				"from-flag/picked/SKILL.md": skillFile("picked", "From flag.", "x"),
			},
			cfg:  map[string]any{"skills_dirs": []string{"from-config"}},
			args: []string{"-skills-dir", "from-flag"},
			want: func(t *testing.T, system string) {
				if !strings.Contains(system, "picked — From flag.") || strings.Contains(system, "From config.") {
					t.Errorf("system prompt lists the wrong skills: %q", system)
				}
			},
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			wd := runtimeWorkdir(t, row.files)
			d, fake := startServeFlags(t, wd, row.cfg, row.env, row.args, replyText("ok"))
			runTurn(t, d, "go")
			row.want(t, fake.Requests()[0].System)
		})
	}
	t.Run("agent_def_dir_flag_replaces_the_config_dirs", func(t *testing.T) {
		t.Parallel()
		const lead = "---\nname: lead\ndescription: Leads.\n---\n\nLead the team.\n"
		wd := runtimeWorkdir(t, map[string]string{"from-flag/lead.md": lead})
		d, fake := startServeFlags(t, wd, map[string]any{"agent_defs_dirs": []string{"from-config"}}, nil, []string{"-agent-def-dir", "from-flag"},
			delegation("lead", harnesstest.Reply{Text: "done"})...)
		runTurn(t, d, "delegate")
		if !fake.AwaitRequests(3, waitBound) {
			t.Fatalf("the child never made a request: %s", requestSummary(fake.Requests()))
		}
		for _, req := range fake.Requests() {
			if strings.Contains(req.System, "Lead the team.") {
				return
			}
		}
		t.Errorf("no child request holds the profile of -agent-def-dir: %s", requestSummary(fake.Requests()))
	})
}
