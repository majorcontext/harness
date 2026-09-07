package config

import (
	"os"
	"path/filepath"
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
	    "timeout_s": 30
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
		{"negative flush", `{"event_sink":{"url":"https://h/x","flush_ms":-1}}`, "flush_ms"},
		{"negative records", `{"event_sink":{"url":"https://h/x","batch_max_records":-1}}`, "batch_max_records"},
		{"negative bytes", `{"event_sink":{"url":"https://h/x","batch_max_bytes":-1}}`, "batch_max_bytes"},
		{"negative timeout", `{"event_sink":{"url":"https://h/x","timeout_s":-1}}`, "timeout_s"},
		{"empty header name", `{"event_sink":{"url":"https://h/x","headers":{"":"v"}}}`, "header name"},
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
