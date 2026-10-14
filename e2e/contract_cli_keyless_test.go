package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

func TestContractCLIRunCompletesATurnOnAProjectProviderWithNoCredentialHeader(t *testing.T) {
	skipShort(t)
	t.Parallel()
	chat := harnesstest.NewChat(t, replyText("local-ok"))
	h := newCLIHost(t, map[string]any{"model": "mine/m"}, replyText("unused"))
	h.env = map[string]string{"OPENAI_API_KEY": "sk-e2e-must-not-leave"}
	project := `{"providers":{"mine":{"type":"openai-compat","base_url":"` + chat.URL() + `"}}}`
	if err := os.WriteFile(filepath.Join(h.work, ".harness.json"), []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := h.run("run", "-p", "hi")
	if code != 0 || !strings.Contains(out, "local-ok") {
		t.Fatalf("run = %d, want a completed turn\n%s\n%s", code, out, errOut)
	}
	reqs := chat.Requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	for _, k := range []string{"Authorization", "X-Api-Key"} {
		if v := reqs[0].Header.Get(k); v != "" {
			t.Errorf("header %s = %q, want none", k, v)
		}
	}
}

func TestContractCLIRunFailsWithTheNoKeyErrorOnAUserProviderWhoseKeyVariableIsUnset(t *testing.T) {
	skipShort(t)
	t.Parallel()
	url, hits, _ := countingServer(t)
	h := newCLIHost(t, map[string]any{"model": "mine/m", "providers": map[string]any{
		"mine": map[string]any{"type": "openai-compat", "base_url": url, "api_key_env": "E2E_UNSET_KEY"}}}, replyText("hi"))
	_, errOut, code := h.run("run", "-p", "hi")
	if want := "no API key: E2E_UNSET_KEY is empty"; code == 0 || !strings.Contains(errOut, want) {
		t.Fatalf("run = %d, want a failure that says %q\n%s", code, want, errOut)
	}
	if hits.Load() != 0 {
		t.Errorf("%d requests reached the endpoint, want none", hits.Load())
	}
}
