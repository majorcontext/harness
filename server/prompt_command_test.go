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

func TestRepositoryPromptCommandExpandsAndKeepsTypedLine(t *testing.T) {
	work := t.TempDir()
	path := filepath.Join(work, ".agents", "commands", "review.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\ndescription: Review a ref\nargument-hint: <ref>\n---\nReview $1; all: $ARGUMENTS; leave $HOME alone."), 0o644); err != nil {
		t.Fatal(err)
	}
	prov := newCapturingProvider(asstTurn("reviewed"))
	dir := t.TempDir()
	srv := newServer(t, dir, prov, 0, func(o *Options) { o.WorkspaceRoots = []string{work} })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	h := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv, ts: ts}
	id := h.createSessionBody(map[string]string{"model": "test/m1", "workdir": work})

	resp, data := h.do("GET", "/commands?workdir="+url.QueryEscape(work), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("commands: %d %s", resp.StatusCode, data)
	}
	var catalog struct {
		Commands []struct{ Name, Kind string } `json:"commands"`
		Support map[string]struct{ Supported bool } `json:"serve_support"`
	}
	if err := json.Unmarshal(data, &catalog); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range catalog.Commands {
		if c.Name == "review" {
			found = c.Kind == "prompt" && catalog.Support[c.Name].Supported
		}
	}
	if !found {
		t.Fatalf("review missing or unsupported in catalog: %s", data)
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
