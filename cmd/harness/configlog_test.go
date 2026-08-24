package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/majorcontext/harness/config"
)

// TestLogConfigSummary_Loaded covers the "operator's config file was
// found and loaded" case: the boot log names its path and the merged
// counts, exactly the line docs/design/managed-processes.md §8
// documents.
func TestLogConfigSummary_Loaded(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logConfigSummary(logger, config.LoadInfo{
		Path:       "/root/web/.harness.json",
		Processes:  2,
		MCPServers: 1,
		Plugins:    0,
	})
	line := buf.String()
	if !strings.Contains(line, `msg="config: /root/web/.harness.json (2 processes, 1 mcp server, 0 plugins)"`) {
		t.Fatalf("log line = %q, want the config summary message", line)
	}
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("log output = %q, want exactly one line", line)
	}
}

// TestLogConfigSummary_NotFound covers the "no config file exists"
// case — the fix's whole point is that this must read differently from
// any loaded-but-empty config, never silently look the same.
func TestLogConfigSummary_NotFound(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logConfigSummary(logger, config.LoadInfo{})
	line := buf.String()
	if !strings.Contains(line, `msg="no config file found"`) {
		t.Fatalf("log line = %q, want the no-config-found message", line)
	}
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("log output = %q, want exactly one line", line)
	}
}

// TestLogConfigSummary_SessionSync covers the backend-aware-durability boot
// line addition: "volume" mode is called out (an operator-relevant
// deviation from the default), while the default "" and the behaviorally
// identical explicit "fsync" are not — the whole point is that only a
// change from built-in default behavior deserves a log line.
func TestLogConfigSummary_SessionSync(t *testing.T) {
	cases := []struct {
		sessionSync string
		wantSuffix  string
	}{
		{"", ""},
		{"fsync", ""},
		{"volume", ", session_sync=volume"},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		logConfigSummary(logger, config.LoadInfo{
			Path:        "/x.json",
			Processes:   1,
			MCPServers:  1,
			Plugins:     1,
			SessionSync: c.sessionSync,
		})
		want := `msg="config: /x.json (1 process, 1 mcp server, 1 plugin)` + c.wantSuffix + `"`
		if !strings.Contains(buf.String(), want) {
			t.Errorf("session_sync %q: log = %q, want it to contain %q", c.sessionSync, buf.String(), want)
		}
	}
}

func TestLogConfigSummary_Pluralization(t *testing.T) {
	cases := []struct {
		info config.LoadInfo
		want string
	}{
		{config.LoadInfo{Path: "/x.json", Processes: 1, MCPServers: 1, Plugins: 1}, "1 process, 1 mcp server, 1 plugin"},
		{config.LoadInfo{Path: "/x.json", Processes: 0, MCPServers: 0, Plugins: 0}, "0 processes, 0 mcp servers, 0 plugins"},
		{config.LoadInfo{Path: "/x.json", Processes: 5, MCPServers: 3, Plugins: 2}, "5 processes, 3 mcp servers, 2 plugins"},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		logConfigSummary(logger, c.info)
		if !strings.Contains(buf.String(), c.want) {
			t.Errorf("for %+v: log = %q, want it to contain %q", c.info, buf.String(), c.want)
		}
	}
}
