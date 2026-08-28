package engine

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/provider"
)

// decodedSessionInfo mirrors the JSON the session_info tool emits.
type decodedSessionInfo struct {
	SessionID    string         `json:"session_id"`
	Model        string         `json:"model"`
	Effort       message.Effort `json:"effort"`
	System       []string       `json:"system"`
	Tools        []string       `json:"tools"`
	Instructions string         `json:"instructions"`
	Skills       []struct {
		Name string `json:"name"`
		Path string `json:"path"`
	} `json:"skills"`
	Plugins []plugin.Info  `json:"plugins"`
	Usage   provider.Usage `json:"usage"`
}

// callSessionInfo runs a session whose model calls session_info on the first
// turn, then returns the decoded tool result AND the raw JSON text the tool
// actually emitted — callers that must assert on exact wire shape (a field's
// presence/absence, an omitempty regression) assert against the raw string,
// never a re-marshaled stand-in, since a local struct's own tags prove
// nothing about the production sessionInfoResult's tags.
func callSessionInfo(t *testing.T, cfg Config) (decodedSessionInfo, string) {
	t.Helper()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("tc1", "session_info", `{}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	cfg.Providers = provider.Registry{"test": prov}
	cfg.Model = message.ModelRef{Provider: "test", Model: "m1"}
	if cfg.System == nil {
		cfg.System = []string{"base"}
	}
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	h := s.History()
	// user, assistant(tool call), tool(result), assistant.
	if len(h) != 4 {
		t.Fatalf("history = %d messages, want 4", len(h))
	}
	tr, ok := h[2].Parts[0].(*message.ToolResult)
	if !ok {
		t.Fatalf("h[2].Parts[0] = %T, want ToolResult", h[2].Parts[0])
	}
	if tr.IsError {
		t.Fatalf("session_info returned an error: %s", tr.Content.Text())
	}
	raw := tr.Content.Text()
	var info decodedSessionInfo
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		t.Fatalf("decoding session_info result %q: %v", raw, err)
	}
	if info.SessionID != s.ID {
		t.Errorf("session_id = %q, want %q", info.SessionID, s.ID)
	}
	return info, raw
}

func TestSessionInfoReportsInjectedContext(t *testing.T) {
	work := t.TempDir()
	writeInstr(t, filepath.Join(work, "AGENTS.md"), "PROJECT_RULE_XYZ applies here")
	skills := filepath.Join(work, "skills")
	writeSkill(t, skills, "demo", "A demo skill")

	info, _ := callSessionInfo(t, Config{WorkDir: work, SkillsDirs: []string{skills}})

	if info.Model != "test/m1" {
		t.Errorf("model = %q, want test/m1", info.Model)
	}
	joined := strings.Join(info.System, "\n")
	if !strings.Contains(joined, "PROJECT_RULE_XYZ applies here") {
		t.Errorf("system missing AGENTS.md content:\n%s", joined)
	}
	if !strings.Contains(joined, "demo — A demo skill") {
		t.Errorf("system missing skill catalog line:\n%s", joined)
	}
	if !strings.Contains(info.Instructions, "AGENTS.md") {
		t.Errorf("instructions provenance = %q, want it to name AGENTS.md", info.Instructions)
	}
	if len(info.Skills) != 1 || info.Skills[0].Name != "demo" {
		t.Fatalf("skills = %+v, want one named demo", info.Skills)
	}
	wantPath := filepath.Join(skills, "demo", "SKILL.md")
	if info.Skills[0].Path != wantPath {
		t.Errorf("skill path = %q, want %q", info.Skills[0].Path, wantPath)
	}
	if !containsStr(info.Tools, "session_info") {
		t.Errorf("tools = %v, want to include session_info", info.Tools)
	}
	if !containsStr(info.Tools, "bash") {
		t.Errorf("tools = %v, want to include bash", info.Tools)
	}
}

func TestSessionInfoNothingInjected(t *testing.T) {
	work := t.TempDir()
	mkdirAll(t, filepath.Join(work, ".git")) // bound the AGENTS.md walk

	info, raw := callSessionInfo(t, Config{
		WorkDir:      work,
		Instructions: &InstructionsConfig{Disabled: true},
		SkillsDirs:   []string{}, // explicit disable
	})

	if info.Instructions != "none" {
		t.Errorf("instructions = %q, want none", info.Instructions)
	}
	if len(info.Skills) != 0 {
		t.Errorf("skills = %+v, want empty", info.Skills)
	}
	// A session with no plugin host reports an empty plugins list, never a
	// null, and does not panic reading it.
	if len(info.Plugins) != 0 {
		t.Errorf("plugins = %+v, want empty", info.Plugins)
	}
	if !strings.Contains(rawSessionInfoPlugins(t, info), "[]") {
		t.Errorf("plugins must serialize as [], got %q", rawSessionInfoPlugins(t, info))
	}
	// System still carries the base segment.
	if len(info.System) != 2 || info.System[0] != "base" || !isBatchingSegment(info.System[1]) {
		t.Errorf("system = %v, want [base, tool-batching]", info.System)
	}
	// Effort was never set: report it honestly as EffortUnset ("", the
	// provider default), not omitted and not an invented level.
	if info.Effort != message.EffortUnset {
		t.Errorf("effort = %q, want EffortUnset", info.Effort)
	}
	// Assert against the RAW production JSON, not a re-marshaled stand-in: a
	// local struct's own tag can't catch a future ",omitempty" added to
	// sessionInfoResult.Effort, since decodedSessionInfo (no omitempty)
	// would still decode a missing key to "" and mask the regression. The
	// tool marshals with MarshalIndent, so the key:value separator carries a
	// space.
	if !strings.Contains(raw, `"effort": ""`) {
		t.Errorf("unset effort must serialize as \"effort\": \"\", got %q", raw)
	}
}

// TestSessionInfoReportsEffort drives the real session_info build function
// with a session created at a non-default reasoning-effort level (mirroring
// the level a session would carry after POST /session/{id}/thinking or a
// create-time Config.Effort) and asserts the level round-trips through
// session_info exactly.
func TestSessionInfoReportsEffort(t *testing.T) {
	work := t.TempDir()
	mkdirAll(t, filepath.Join(work, ".git"))

	info, _ := callSessionInfo(t, Config{
		WorkDir:      work,
		Instructions: &InstructionsConfig{Disabled: true},
		SkillsDirs:   []string{},
		Effort:       message.EffortHigh,
	})

	if info.Effort != message.EffortHigh {
		t.Errorf("effort = %q, want %q", info.Effort, message.EffortHigh)
	}
}

// TestSessionInfoReportsEffortAfterSetEffort proves session_info reflects a
// SetEffort swap made after the session was created — the same path
// handleSetThinking (POST /session/{id}/thinking) drives — not just the
// create-time Config.Effort.
func TestSessionInfoReportsEffortAfterSetEffort(t *testing.T) {
	work := t.TempDir()
	mkdirAll(t, filepath.Join(work, ".git"))

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("tc1", "session_info", `{}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers:    provider.Registry{"test": prov},
		Model:        message.ModelRef{Provider: "test", Model: "m1"},
		WorkDir:      work,
		Instructions: &InstructionsConfig{Disabled: true},
		SkillsDirs:   []string{},
	})
	s.SetEffort(message.EffortLow)

	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	h := s.History()
	tr, ok := h[2].Parts[0].(*message.ToolResult)
	if !ok {
		t.Fatalf("h[2].Parts[0] = %T, want ToolResult", h[2].Parts[0])
	}
	var info decodedSessionInfo
	if err := json.Unmarshal([]byte(tr.Content.Text()), &info); err != nil {
		t.Fatalf("decoding session_info result %q: %v", tr.Content.Text(), err)
	}
	if info.Effort != message.EffortLow {
		t.Errorf("effort = %q, want %q", info.Effort, message.EffortLow)
	}
}

// rawSessionInfoPlugins re-marshals just the plugins field so the test can
// assert the empty case serializes as [] (not null).
func rawSessionInfoPlugins(t *testing.T, info decodedSessionInfo) string {
	t.Helper()
	b, err := json.Marshal(info.Plugins)
	if err != nil {
		t.Fatalf("marshal plugins: %v", err)
	}
	return string(b)
}

// TestSessionInfoReportsConfiguredPlugins drives the real session_info build
// function (Session.sessionInfo — the exact call the tool's Run makes) and
// asserts a configured-but-not-yet-spawned plugin is reported with its name,
// spawn state, registered tools, and subscribed hooks. It uses a real
// plugin.Host over an in-process pipe (plugin.NewTestSpec), never a
// subprocess. It calls sessionInfo directly, without a model turn, so no hook
// dispatch spawns the plugin — proving the lazy, not-yet-spawned plugin still
// appears (the primary requirement).
func TestSessionInfoReportsConfiguredPlugins(t *testing.T) {
	spec := plugin.NewTestSpec("guard", &plugin.Hooks{
		ChatParams: func(context.Context, *plugin.Client, *plugin.ChatParamsRequest) (*plugin.ChatParamsResponse, error) {
			return nil, nil
		},
		SystemTransform: func(context.Context, *plugin.Client, *plugin.SystemTransformRequest) (*plugin.SystemTransformResponse, error) {
			return nil, nil
		},
		Tools: []plugin.Tool{{
			Def: plugin.ToolDef{Name: "scan_file", Description: "d", InputSchema: json.RawMessage(`{}`)},
			Execute: func(context.Context, *plugin.Client, json.RawMessage) (message.Parts, error) {
				return nil, nil
			},
		}},
	})
	host, err := plugin.NewHost(plugin.Options{}, spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Close)

	s := NewSession(Config{
		Providers: provider.Registry{"test": &scriptedProvider{name: "test"}},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		Hooks:     host,
	})
	info := s.sessionInfo(context.Background())

	if len(info.Plugins) != 1 {
		t.Fatalf("plugins = %+v, want exactly one", info.Plugins)
	}
	p := info.Plugins[0]
	if p.Name != "guard" {
		t.Errorf("plugin name = %q, want guard", p.Name)
	}
	// Lazy spawn: reading session_info must not have spawned the plugin.
	if p.State != plugin.PluginNotSpawned {
		t.Errorf("plugin state = %q, want %q", p.State, plugin.PluginNotSpawned)
	}
	// Surplus direction: the plugin's tools and hooks are actually listed.
	if !containsStr(p.Tools, "scan_file") {
		t.Errorf("plugin tools = %v, want to include scan_file", p.Tools)
	}
	if !containsStr(p.Hooks, "chat.params") || !containsStr(p.Hooks, "system.transform") {
		t.Errorf("plugin hooks = %v, want chat.params and system.transform", p.Hooks)
	}
}
