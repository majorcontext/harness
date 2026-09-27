package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

func TestRepositoryCommandRejectsEmptyExpansion(t *testing.T) {
	work := t.TempDir()
	path := filepath.Join(work, ".agents", "commands", "review.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\ndescription: Review\n---\n$1"), 0o644); err != nil {
		t.Fatal(err)
	}
	prov := newCapturingProvider(asstTurn("ok"))
	dir := t.TempDir()
	srv := newServer(t, dir, prov, 0, func(o *Options) { o.WorkspaceRoots = []string{work} })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	h := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv, ts: ts}
	id := h.createSessionBody(map[string]string{"model": "test/m1", "workdir": work})
	for _, route := range []string{"prompt_async", "enqueue"} {
		body := map[string]any{"parts": []map[string]string{{"type": "text", "text": "/review"}}, "source": "typed"}
		if route == "enqueue" {
			body["seq"] = int64(1)
		}
		resp, data := h.do("POST", "/session/"+id+"/"+route, body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s empty expansion: %d %s, want 400", route, resp.StatusCode, data)
		}
	}
	if users := h.userMessages(id); len(users) != 0 {
		t.Fatalf("empty expansion appended user messages: %+v", users)
	}
}

func TestRepositoryCommandEnqueueRetrySkipsInvalidFile(t *testing.T) {
	work := t.TempDir()
	path := filepath.Join(work, ".agents", "commands", "review.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\ndescription: Review\n---\nReview $ARGUMENTS"), 0o644); err != nil {
		t.Fatal(err)
	}
	prov := newCapturingProvider(asstTurn("ok"))
	dir := t.TempDir()
	srv := newServer(t, dir, prov, 0, func(o *Options) { o.WorkspaceRoots = []string{work} })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	h := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv, ts: ts}
	id := h.createSessionBody(map[string]string{"model": "test/m1", "workdir": work})
	body := map[string]any{"parts": []map[string]string{{"type": "text", "text": "/review HEAD"}}, "seq": int64(9), "source": "typed"}
	resp, data := h.do("POST", "/session/"+id+"/enqueue", body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first enqueue: %d %s", resp.StatusCode, data)
	}
	h.waitIdle(id)
	if err := os.WriteFile(path, []byte("broken frontmatter"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, data = h.do("POST", "/session/"+id+"/enqueue", body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), `"status":"duplicate"`) {
		t.Fatalf("retry after file changed: %d %s, want duplicate", resp.StatusCode, data)
	}
}

func TestRepositoryPromptCommandSanitizesTypedLine(t *testing.T) {
	work := t.TempDir()
	path := filepath.Join(work, ".agents", "commands", "review.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\ndescription: Review\n---\nReview $ARGUMENTS"), 0o644); err != nil {
		t.Fatal(err)
	}
	prov := newCapturingProvider(asstTurn("ok"))
	dir := t.TempDir()
	srv := newServer(t, dir, prov, 0, func(o *Options) { o.WorkspaceRoots = []string{work} })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	h := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv, ts: ts}
	id := h.createSessionBody(map[string]string{"model": "test/m1", "workdir": work})
	line := "/review " + strings.Repeat("a", 300) + "\u202e"
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": line}}, "source": "typed",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async: %d %s", resp.StatusCode, data)
	}
	h.waitIdle(id)
	users := h.userMessages(id)
	if len(users) != 1 || len(users[0].SourceLabel) > sourceLabelMaxBytes || strings.ContainsRune(users[0].SourceLabel, '\u202e') {
		t.Fatalf("unsafe source label: %+v", users)
	}
}

func TestInvalidRepositoryCommandNameStaysOrdinaryPrompt(t *testing.T) {
	prov := newCapturingProvider(asstTurn("ok"))
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "/review.bad"}}, "source": "typed",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async: %d %s", resp.StatusCode, data)
	}
	h.waitIdle(id)
	users := h.userMessages(id)
	if len(users) != 1 || users[0].Parts.Text() != "/review.bad" {
		t.Fatalf("user messages = %+v, want literal unknown command", users)
	}
}

func TestRepositoryPromptCommandExpandsAndKeepsTypedLine(t *testing.T) {
	work := t.TempDir()
	path := filepath.Join(work, ".agents", "commands", "review.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\ndescription: Review a ref\nargument-hint: <ref>\narguments:\n  - name: ref\n    description: Ref to review\n    required: true\n---\nReview $1; all: $ARGUMENTS; leave $HOME alone."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "broken.md"), []byte("---\ndescription: Broken\nunsupported: value\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "clear.md"), []byte("---\ndescription: Collision\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	prov := newCapturingProvider(asstTurn("reviewed"))
	dir := t.TempDir()
	srv := newServer(t, dir, prov, 0, func(o *Options) { o.WorkspaceRoots = []string{work} })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	h := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv, ts: ts}
	id := h.createSessionBody(map[string]string{"model": "test/m1", "workdir": work})

	resp, data := h.do("GET", "/commands?workdir="+url.QueryEscape(t.TempDir()), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("commands outside workspace: %d %s, want 400", resp.StatusCode, data)
	}
	resp, data = h.do("GET", "/commands?workdir="+url.QueryEscape(work), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("commands: %d %s", resp.StatusCode, data)
	}
	var catalog struct {
		Commands []struct{ Name, Kind string } `json:"commands"`
		Support  map[string]struct {
			Supported bool   `json:"supported"`
			Reason    string `json:"reason"`
		} `json:"serve_support"`
		DiscoveryErrors []string `json:"discovery_errors"`
	}
	if err := json.Unmarshal(data, &catalog); err != nil {
		t.Fatal(err)
	}
	found, broken := false, false
	for _, c := range catalog.Commands {
		if c.Name == "review" {
			found = c.Kind == "prompt" && catalog.Support[c.Name].Supported
		}
		if c.Name == "broken" {
			broken = c.Kind == "prompt" && !catalog.Support[c.Name].Supported && strings.Contains(catalog.Support[c.Name].Reason, "unsupported")
		}
	}
	if !found || !broken || len(catalog.DiscoveryErrors) != 1 || !strings.Contains(catalog.DiscoveryErrors[0], "clear") {
		t.Fatalf("review, broken command, or collision error missing from catalog: %s", data)
	}

	line := "/review HEAD~1"
	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": line}}, "source": "typed",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async: %d %s", resp.StatusCode, data)
	}
	h.waitIdle(id)
	users := h.userMessages(id)
	if len(users) != 1 || strings.TrimSpace(users[0].Parts.Text()) != "Review HEAD~1; all: HEAD~1; leave $HOME alone." {
		t.Fatalf("expanded user messages = %+v", users)
	}
	if users[0].Source != message.PromptSourceCommand || users[0].SourceLabel != line {
		t.Fatalf("provenance = %+v, want command source and original line", users[0])
	}
	if commands := h.sessionDirect(id).Commands(); len(commands) != 0 {
		t.Fatalf("prompt command recorded %d control results", len(commands))
	}
}
