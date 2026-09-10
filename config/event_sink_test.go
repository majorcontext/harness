package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeSinkConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "harness.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadAcceptsEventSink(t *testing.T) {
	path := writeSinkConfig(t, `{
	  "event_sink": {
	    "url": "https://example.test/v1/journal",
	    "headers": {"Authorization": "Bearer t"},
	    "generation": "jrnl_01h455vb4pex5vsknk084sn02q",
	    "flush_ms": 250,
	    "batch_max_records": 256,
	    "batch_max_bytes": 4194304,
	    "timeout_s": 30,
	    "include_types": ["prompt.queued", "prompt.dequeued", "turn.end"]
	  }
	}`)

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.EventSink == nil {
		t.Fatal("EventSink is nil, want the parsed block")
	}
	if c.EventSink.URL != "https://example.test/v1/journal" {
		t.Errorf("URL = %q", c.EventSink.URL)
	}
	if c.EventSink.Headers["Authorization"] != "Bearer t" {
		t.Errorf("Headers = %v", c.EventSink.Headers)
	}
	if c.EventSink.Generation != "jrnl_01h455vb4pex5vsknk084sn02q" {
		t.Errorf("Generation = %q", c.EventSink.Generation)
	}
	if c.EventSink.FlushMS != 250 || c.EventSink.BatchMaxRecords != 256 ||
		c.EventSink.BatchMaxBytes != 4194304 || c.EventSink.TimeoutS != 30 {
		t.Errorf("numeric fields = %+v", c.EventSink)
	}
	wantTypes := []string{"prompt.queued", "prompt.dequeued", "turn.end"}
	if !reflect.DeepEqual(c.EventSink.IncludeTypes, wantTypes) {
		t.Errorf("IncludeTypes = %#v, want %#v", c.EventSink.IncludeTypes, wantTypes)
	}
}

// TestLoadAcceptsEventSinkWithoutIncludeTypes pins the unfiltered default. An
// absent list and an explicit empty list both mean "forward every record", so
// neither may fail validation and neither may report a selection.
func TestLoadAcceptsEventSinkWithoutIncludeTypes(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"absent list", `{"event_sink":{"url":"https://h/x"}}`},
		{"explicit empty list", `{"event_sink":{"url":"https://h/x","include_types":[]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(writeSinkConfig(t, tc.body))
			if err != nil {
				t.Fatalf("Load(%s): %v", tc.body, err)
			}
			if len(c.EventSink.IncludeTypes) != 0 {
				t.Errorf("Load(%s): IncludeTypes = %#v, want an empty selection (unfiltered)", tc.body, c.EventSink.IncludeTypes)
			}
		})
	}
}

func TestLoadProjectMergesEventSink(t *testing.T) {
	userPath := writeSinkConfig(t, `{"model":"anthropic/user-model"}`)
	t.Setenv("HARNESS_CONFIG", userPath)

	projectDir := t.TempDir()
	project := `{
	  "event_sink": {
	    "url": "https://example.test/v1/journal",
	    "headers": {"Authorization": "Bearer project"},
	    "generation": "jrnl_project",
	    "flush_ms": 125,
	    "batch_max_records": 64,
	    "batch_max_bytes": 1048576,
	    "timeout_s": 15,
	    "include_types": ["prompt.queued", "prompt.dequeued", "turn.end"]
	  }
	}`
	if err := os.WriteFile(filepath.Join(projectDir, ".harness.json"), []byte(project), 0o600); err != nil {
		t.Fatalf("write project config: %v", err)
	}

	c, err := LoadProject(projectDir)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if c.EventSink == nil {
		t.Fatal("EventSink is nil; project event_sink was lost while merging the managed user config")
	}
	want := EventSinkSpec{
		URL:             "https://example.test/v1/journal",
		Headers:         map[string]string{"Authorization": "Bearer project"},
		Generation:      "jrnl_project",
		FlushMS:         125,
		BatchMaxRecords: 64,
		BatchMaxBytes:   1048576,
		TimeoutS:        15,
		IncludeTypes:    []string{"prompt.queued", "prompt.dequeued", "turn.end"},
	}
	if !reflect.DeepEqual(*c.EventSink, want) {
		t.Errorf("EventSink = %+v, want %+v", *c.EventSink, want)
	}
}

func TestMergeEventSink(t *testing.T) {
	base := &Config{EventSink: &EventSinkSpec{
		URL:        "https://user.test/journal",
		Headers:    map[string]string{"Authorization": "Bearer user"},
		Generation: "jrnl_user",
		FlushMS:    250,
	}}
	override := &Config{EventSink: &EventSinkSpec{
		URL:        "https://project.test/journal",
		Headers:    map[string]string{"Authorization": "Bearer project"},
		Generation: "jrnl_project",
		TimeoutS:   15,
	}}

	t.Run("absent project block inherits without aliasing", func(t *testing.T) {
		got := merge(base, &Config{})
		if got.EventSink == nil || !reflect.DeepEqual(*got.EventSink, *base.EventSink) {
			t.Fatalf("EventSink = %+v, want inherited %+v", got.EventSink, base.EventSink)
		}
		got.EventSink.Headers["Authorization"] = "changed"
		if base.EventSink.Headers["Authorization"] != "Bearer user" {
			t.Fatal("merged EventSink.Headers aliases the user config")
		}
	})

	t.Run("project block replaces wholesale without aliasing", func(t *testing.T) {
		got := merge(base, override)
		if got.EventSink == nil || !reflect.DeepEqual(*got.EventSink, *override.EventSink) {
			t.Fatalf("EventSink = %+v, want project override %+v", got.EventSink, override.EventSink)
		}
		got.EventSink.Headers["Authorization"] = "changed"
		if override.EventSink.Headers["Authorization"] != "Bearer project" {
			t.Fatal("merged EventSink.Headers aliases the project config")
		}
	})

	t.Run("explicit empty headers do not alias", func(t *testing.T) {
		over := &Config{EventSink: &EventSinkSpec{
			URL:     "https://project.test/journal",
			Headers: map[string]string{},
		}}
		got := merge(&Config{}, over)
		if got.EventSink == nil || got.EventSink.Headers == nil {
			t.Fatalf("EventSink = %+v, want a non-nil empty Headers map", got.EventSink)
		}
		got.EventSink.Headers["X-Test"] = "changed"
		if len(over.EventSink.Headers) != 0 {
			t.Fatal("merged empty EventSink.Headers aliases the project config")
		}
	})
}

func TestLoadOmitsEventSinkWhenAbsent(t *testing.T) {
	c, err := Load(writeSinkConfig(t, `{"model": "a/b"}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.EventSink != nil {
		t.Fatalf("EventSink = %+v, want nil when the block is absent", c.EventSink)
	}
}

func TestLoadRejectsBadEventSink(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty url", `{"event_sink":{"url":""}}`, "url is required"},
		{"unparseable url", `{"event_sink":{"url":"://nope"}}`, "url"},
		{"non-http scheme", `{"event_sink":{"url":"ftp://h/x"}}`, "http or https"},
		{"missing host", `{"event_sink":{"url":"https:///path"}}`, "host is required"},
		{"embedded credentials", `{"event_sink":{"url":"https://user:pass@example.test/path"}}`, "must not include userinfo"},
		{"negative flush", `{"event_sink":{"url":"https://h/x","flush_ms":-1}}`, "flush_ms"},
		{"negative records", `{"event_sink":{"url":"https://h/x","batch_max_records":-1}}`, "batch_max_records"},
		{"negative bytes", `{"event_sink":{"url":"https://h/x","batch_max_bytes":-1}}`, "batch_max_bytes"},
		{"negative timeout", `{"event_sink":{"url":"https://h/x","timeout_s":-1}}`, "timeout_s"},
		{"empty header name", `{"event_sink":{"url":"https://h/x","headers":{"":"v"}}}`, "header name"},
		{"empty include type", `{"event_sink":{"url":"https://h/x","include_types":["turn.end",""]}}`, "include_types"},
		{"duplicate include type", `{"event_sink":{"url":"https://h/x","include_types":["turn.end","turn.end"]}}`, "duplicate"},
		{"leading whitespace include type", `{"event_sink":{"url":"https://h/x","include_types":[" turn.end"]}}`, "whitespace"},
		{"trailing whitespace include type", `{"event_sink":{"url":"https://h/x","include_types":["turn.end\n"]}}`, "whitespace"},
		{"whitespace-only include type", `{"event_sink":{"url":"https://h/x","include_types":["  "]}}`, "whitespace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeSinkConfig(t, tc.body))
			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestMergeEventSinkIncludeTypes pins the selector list through the user and
// project merge. The pump reads only the merged slice: a merge that dropped it
// would forward every record while the config asks for three types, and one
// that aliased an input would let a later edit of that layer reach the running
// pump.
func TestMergeEventSinkIncludeTypes(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"absent list stays unfiltered", nil, nil},
		{"explicit empty list stays unfiltered", []string{}, []string{}},
		{"selector list survives", []string{"prompt.queued", "prompt.dequeued", "turn.end"}, []string{"prompt.queued", "prompt.dequeued", "turn.end"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			over := &Config{EventSink: &EventSinkSpec{
				URL:          "https://project.test/journal",
				IncludeTypes: tc.in,
			}}
			got := merge(&Config{}, over)
			if got.EventSink == nil {
				t.Fatal("EventSink is nil, want the project block")
			}
			if !reflect.DeepEqual(got.EventSink.IncludeTypes, tc.want) {
				t.Fatalf("IncludeTypes = %#v, want %#v", got.EventSink.IncludeTypes, tc.want)
			}
			if len(tc.in) == 0 {
				return
			}
			got.EventSink.IncludeTypes[0] = "changed"
			if over.EventSink.IncludeTypes[0] != "prompt.queued" {
				t.Fatal("merged EventSink.IncludeTypes aliases the project config")
			}
		})
	}

	t.Run("absent project block inherits the user list without aliasing", func(t *testing.T) {
		base := &Config{EventSink: &EventSinkSpec{
			URL:          "https://user.test/journal",
			IncludeTypes: []string{"turn.end"},
		}}
		got := merge(base, &Config{})
		if got.EventSink == nil || !reflect.DeepEqual(got.EventSink.IncludeTypes, []string{"turn.end"}) {
			t.Fatalf("IncludeTypes = %#v, want the inherited user list", got.EventSink)
		}
		got.EventSink.IncludeTypes[0] = "changed"
		if base.EventSink.IncludeTypes[0] != "turn.end" {
			t.Fatal("merged EventSink.IncludeTypes aliases the user config")
		}
	})
}
