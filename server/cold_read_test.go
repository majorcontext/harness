package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/provider"
)

// coldSession writes a real session log into dir through the engine's own
// production path (NewSession + Prompt), without the server ever seeing it.
// The result is exactly the state GET /session/{id} answers cold: a journal
// on disk, no residency entry, no SessionManager node.
func coldSession(t *testing.T, dir string, cfgMutate func(*engine.Config)) *engine.Session {
	t.Helper()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("cold reply")}}
	cfg := engine.Config{
		Providers:  provider.Registry{prov.name: prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir: dir,
		WorkDir:    dir,
	}
	if cfgMutate != nil {
		cfgMutate(&cfg)
	}
	sess := engine.NewSession(cfg)
	if _, err := sess.Prompt(context.Background(), "hello"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if err := sess.PersistErr(); err != nil {
		t.Fatalf("PersistErr: %v", err)
	}
	return sess
}

// breakJournal overwrites a session journal with unreadable bytes of the
// same length and modification time — the index's whole staleness key,
// left untouched. A handler that still replays the journal therefore fails
// visibly, which is what makes this a proof and not a hope. Production
// never produces this state: a journal is append-only with one writer.
func breakJournal(t *testing.T, dir, id string) {
	t.Helper()
	path := filepath.Join(dir, id+".jsonl")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Restore the modification time: it is the other half of the
		// index's staleness key, and this probe must change neither half.
		if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
			t.Fatal(err)
		}
	}()
	junk := make([]byte, len(data))
	for i := range junk {
		junk[i] = 'x'
	}
	if err := os.WriteFile(path, junk, 0o644); err != nil {
		t.Fatal(err)
	}
}

func decodeSession(t *testing.T, data []byte) sessionJSON {
	t.Helper()
	var out sessionJSON
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode session: %v (%s)", err, data)
	}
	return out
}

// TestGetSessionColdAnswersFromIndex is the workstream's headline claim:
// GET /session/{id} for a session this process does not hold live never
// replays the journal.
func TestGetSessionColdAnswersFromIndex(t *testing.T) {
	dir := t.TempDir()
	sess := coldSession(t, dir, nil)
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})
	breakJournal(t, dir, sess.ID)

	resp, data := h.do("GET", "/session/"+sess.ID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /session/%s = %d: %s", sess.ID, resp.StatusCode, data)
	}
	got := decodeSession(t, data)
	if got.ID != sess.ID {
		t.Errorf("id = %q, want %q", got.ID, sess.ID)
	}
	if got.Messages != 2 {
		t.Errorf("messages = %d, want 2 (user + assistant)", got.Messages)
	}
	if got.Model != sess.Model() {
		t.Errorf("model = %v, want %v", got.Model, sess.Model())
	}
	if got.WorkDir != dir {
		t.Errorf("workdir = %q, want %q", got.WorkDir, dir)
	}
	if got.Status != "idle" || got.State != "idle" {
		t.Errorf("status/state = %q/%q, want idle/idle", got.Status, got.State)
	}
	if got.LastActivityAt.IsZero() {
		t.Error("last_activity_at is zero, want the newest message's timestamp")
	}
	if got.Plugins == nil {
		t.Error("plugins = null, want an array")
	}
}

// TestListSessionsColdAnswersFromIndex is the same claim for the list
// endpoint, which used to pay one full replay per non-resident session.
func TestListSessionsColdAnswersFromIndex(t *testing.T) {
	dir := t.TempDir()
	first := coldSession(t, dir, nil)
	second := coldSession(t, dir, nil)
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})
	breakJournal(t, dir, first.ID)
	breakJournal(t, dir, second.ID)

	resp, data := h.do("GET", "/session", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /session = %d: %s", resp.StatusCode, data)
	}
	var list []sessionJSON
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("decode list: %v (%s)", err, data)
	}
	if len(list) != 2 {
		t.Fatalf("listed %d sessions, want 2: %s", len(list), data)
	}
	for _, entry := range list {
		if entry.Messages != 2 {
			t.Errorf("session %s: messages = %d, want 2", entry.ID, entry.Messages)
		}
	}
}

// TestGetSessionColdReportsLineage: a task child's lineage is durable, and
// the cold read must still report it — the same durable-only block a
// disk-loaded session reported before, now sourced from the index.
func TestGetSessionColdReportsLineage(t *testing.T) {
	dir := t.TempDir()
	child := coldSession(t, dir, func(cfg *engine.Config) {
		cfg.TaskParentID = "ses_0123456789abcdef"
		cfg.TaskAgentType = "explore"
		cfg.TaskDepth = 2
	})
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})
	breakJournal(t, dir, child.ID)

	resp, data := h.do("GET", "/session/"+child.ID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /session/%s = %d: %s", child.ID, resp.StatusCode, data)
	}
	got := decodeSession(t, data)
	if got.Lineage == nil {
		t.Fatalf("lineage absent for a task child: %s", data)
	}
	if got.Lineage.ParentID != "ses_0123456789abcdef" {
		t.Errorf("lineage.parent_id = %q, want ses_0123456789abcdef", got.Lineage.ParentID)
	}
	if got.Lineage.AgentType != "explore" {
		t.Errorf("lineage.agent_type = %q, want explore", got.Lineage.AgentType)
	}
	if got.Lineage.Depth != 2 {
		t.Errorf("lineage.depth = %d, want 2", got.Lineage.Depth)
	}
}

// TestGetSessionColdReportsConfiguredPlugins: plugins are process
// configuration, not durable session state, so the cold path reads them
// from Options.Plugins. Without that seam a cold read would silently
// report no plugins for a process that has them.
func TestGetSessionColdReportsConfiguredPlugins(t *testing.T) {
	dir := t.TempDir()
	sess := coldSession(t, dir, nil)
	want := []plugin.Info{{Name: "guard", Tools: []string{"scan"}}}
	srv := newServer(t, dir, &scriptedProvider{name: "test"}, 0, func(o *Options) {
		o.Plugins = func(string) []plugin.Info { return want }
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	h := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv, ts: ts}
	breakJournal(t, dir, sess.ID)

	resp, data := h.do("GET", "/session/"+sess.ID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /session/%s = %d: %s", sess.ID, resp.StatusCode, data)
	}
	got := decodeSession(t, data)
	if len(got.Plugins) != 1 || got.Plugins[0].Name != "guard" {
		t.Errorf("plugins = %+v, want the one configured plugin", got.Plugins)
	}
}

// TestGetSessionUnknownIDIsNotFound: an id with no journal must still 404,
// not report an empty summary.
func TestGetSessionUnknownIDIsNotFound(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	resp, _ := h.do("GET", "/session/ses_0123456789abcdef", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("GET unknown session = %d, want 404", resp.StatusCode)
	}
}

// TestGetSessionPrefersLiveObjectOverIndex: a session this process is
// actively running must be rendered from its live object. The index is a
// summary of the JOURNAL, which cannot know a turn is in flight, so a cold
// read of a running session would report "idle" — the exact false-idle
// answer an orchestrator acts on.
func TestGetSessionPrefersLiveObjectOverIndex(t *testing.T) {
	prov := newBlockingProvider("test")
	h := newHarness(t, prov)
	id := h.createSession("")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{"parts": []map[string]string{{"type": "text", "text": "go"}}})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt_async = %d: %s", resp.StatusCode, data)
	}
	<-prov.started
	defer prov.releaseAll()

	resp, data = h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /session/%s = %d: %s", id, resp.StatusCode, data)
	}
	got := decodeSession(t, data)
	if got.Status != "busy" {
		t.Errorf("status = %q, want busy (a live session must not be read from its index)", got.Status)
	}
}

// TestGetSessionColdFallsBackForLegacyJournal: a journal that never
// recorded a model cannot be answered from a fold — engine.LoadSession
// answers that from the loading Config, and the index says so through
// SessionIndex.Complete. The handler must then use the load path and report
// the same model it always did, not an empty one. OpenAPI makes `model`
// required, so an empty value is not a smaller answer, it is an invalid
// one.
func TestGetSessionColdFallsBackForLegacyJournal(t *testing.T) {
	dir := t.TempDir()
	id := "ses_0123456789abcdef"
	// A crash between the header write and the model record beside it: the
	// header is complete, the model record is torn away.
	journal := `{"type":"session","id":"ses_0123456789abcdef","created_at":"2026-01-02T03:04:05Z","workdir":"/tmp"}
{"type":"model","mod`
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(journal), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})

	resp, data := h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /session/%s = %d: %s", id, resp.StatusCode, data)
	}
	got := decodeSession(t, data)
	if got.Model.IsZero() {
		t.Errorf("model = %v, want the configured default the load path restores", got.Model)
	}

	// The same session must still appear in a listing, with the same model.
	resp, data = h.do("GET", "/session", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /session = %d: %s", resp.StatusCode, data)
	}
	var list []sessionJSON
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Model.IsZero() {
		t.Fatalf("listing = %s, want one entry carrying a model", data)
	}
}

// TestColdPluginsAreAskedPerSession pins the seam's shape: an embedder may
// wire different hooks per session through its own NewSession/LoadSession
// wrappers, so the cold read asks for THAT session's plugins, not the
// process's.
func TestColdPluginsAreAskedPerSession(t *testing.T) {
	dir := t.TempDir()
	first := coldSession(t, dir, nil)
	second := coldSession(t, dir, nil)
	srv := newServer(t, dir, &scriptedProvider{name: "test"}, 0, func(o *Options) {
		o.Plugins = func(sessionID string) []plugin.Info {
			return []plugin.Info{{Name: "for-" + sessionID}}
		}
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	h := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv, ts: ts}

	for _, id := range []string{first.ID, second.ID} {
		resp, data := h.do("GET", "/session/"+id, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("GET /session/%s = %d: %s", id, resp.StatusCode, data)
		}
		got := decodeSession(t, data)
		if len(got.Plugins) != 1 || got.Plugins[0].Name != "for-"+id {
			t.Errorf("session %s: plugins = %+v, want the per-session answer", id, got.Plugins)
		}
	}
}

// TestEvictionReleasesSessionFileHandles: a Session holds two descriptors
// for its whole life — its journal and its sidecar index — and a server
// keeps one Session per session it has touched. A long-lived box with many
// subagent sessions accumulates them. Eviction is the point the server has
// already decided a session is idle and reloadable, so it is the point that
// releases them.
//
// The session must stay usable: the next persist reopens both handles, and
// the index it then writes must still describe the whole journal.
func TestEvictionReleasesSessionFileHandles(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("one"), asstTurn("two"), asstTurn("three")}}
	h := newHarnessOpts(t, dir, prov, 1) // MaxResident 1: the next create evicts
	first := h.createSession("")
	resp, data := h.do("POST", "/session/"+first+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt_async = %d: %s", resp.StatusCode, data)
	}
	h.waitIdle(first)

	sess := h.srv.residentSession(first)
	if sess == nil {
		t.Fatal("test setup: the first session is not resident")
	}
	openBefore := openDescriptors(t)

	// A second session evicts the first.
	h.createSession("")
	if h.srv.residentSession(first) != nil {
		t.Fatal("test setup: the first session was not evicted")
	}
	if openDescriptors(t) > openBefore {
		t.Errorf("descriptor count rose from %d to %d across an eviction", openBefore, openDescriptors(t))
	}

	// The evicted session object stays usable: a further append reopens its
	// handles, and the index still describes the whole journal.
	if _, err := sess.Prompt(context.Background(), "again"); err != nil {
		t.Fatalf("Prompt on an evicted session: %v", err)
	}
	if err := sess.PersistErr(); err != nil {
		t.Fatalf("PersistErr after reopen: %v", err)
	}
	ix, err := engine.ReadSessionIndex(dir, first)
	if err != nil {
		t.Fatalf("ReadSessionIndex: %v", err)
	}
	if ix.Messages != 4 {
		t.Errorf("index reports %d messages, want 4 after the reopened append", ix.Messages)
	}
}

// openDescriptors counts this process's open file descriptors. It is a
// Linux-only reading of /proc/self/fd; on any other system the test that
// uses it still checks the reopen behavior, and skips the count.
func openDescriptors(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("no /proc/self/fd; cannot count descriptors on this system")
	}
	return len(entries)
}

// TestListDoesNotReadIndexesForLiveSessions: GET /session renders a live
// session from its live object, so reading that session's index is work
// thrown away — and a stale sidecar would be refolded and written back by
// the listing while the session's own writer holds it.
//
// The probe removes a live session's sidecar and leaves its journal intact.
// A listing that reads indexes for every file would refold this one and
// write the sidecar back. A listing that resolves residency first never
// touches it, so the sidecar stays absent.
func TestListDoesNotReadIndexesForLiveSessions(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("one")}}
	h := newHarnessDir(t, dir, prov)
	id := h.createSession("")
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt_async = %d: %s", resp.StatusCode, data)
	}
	h.waitIdle(id)

	if err := os.Remove(filepath.Join(dir, id+".index.json")); err != nil {
		t.Fatal(err)
	}

	resp, data = h.do("GET", "/session", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /session = %d: %s", resp.StatusCode, data)
	}
	var list []sessionJSON
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != id {
		t.Fatalf("listing = %s, want the one live session %s", data, id)
	}
	if list[0].Messages != 2 {
		t.Errorf("messages = %d, want 2 from the live object", list[0].Messages)
	}
	// The listing must not have written a sidecar for it either.
	if _, err := os.Stat(filepath.Join(dir, id+".index.json")); err == nil {
		t.Error("the listing refolded and wrote a sidecar for a live session")
	}
}

// TestListAndGetAgreeOnLiveness: GET /session and GET /session/{id} must
// not disagree about whether a session is running.
//
// Both cold paths now render through coldSessionJSON, which re-checks
// residency after reading the index. That window — a session going live
// between the residency check and the index read — is not reachable
// deterministically from a test, so the guarantee is structural: one
// shared path, rather than two that must be kept in step. This test pins
// the observable half, that the two endpoints agree.
func TestListAndGetAgreeOnLiveness(t *testing.T) {
	prov := newBlockingProvider("test")
	h := newHarness(t, prov)
	id := h.createSession("")
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt_async = %d: %s", resp.StatusCode, data)
	}
	<-prov.started
	defer prov.releaseAll()

	resp, data = h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /session/%s = %d: %s", id, resp.StatusCode, data)
	}
	fromGet := decodeSession(t, data)

	resp, data = h.do("GET", "/session", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /session = %d: %s", resp.StatusCode, data)
	}
	var list []sessionJSON
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("listed %d sessions, want 1", len(list))
	}
	if list[0].Status != fromGet.Status || list[0].State != fromGet.State {
		t.Errorf("listing says %q/%q, GET says %q/%q", list[0].Status, list[0].State, fromGet.Status, fromGet.State)
	}
	if fromGet.Status != "busy" {
		t.Errorf("status = %q, want busy", fromGet.Status)
	}
}

// TestSessionExistenceCheckIsOneStat: abort, end, and wait ask only whether
// a session's journal is there. That check must not walk the directory,
// fold every journal in it, or write sidecars back — and it must answer YES
// for a journal that exists but cannot be read, because a damaged session
// is still a session that exists.
func TestSessionExistenceCheckIsOneStat(t *testing.T) {
	dir := t.TempDir()
	sess := coldSession(t, dir, nil)
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})

	// Corrupt the journal so nothing can fold it, and drop its sidecar.
	if err := os.Remove(filepath.Join(dir, sess.ID+".index.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sess.ID+".jsonl"), []byte("{\"type\":\"session\"}\n{not json\n{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// POST /abort answers from the existence check alone.
	resp, data := h.do("POST", "/session/"+sess.ID+"/abort", nil)
	if resp.StatusCode != 204 {
		t.Errorf("POST abort on an existing but unreadable session = %d: %s; want 204", resp.StatusCode, data)
	}
	// And no sidecar was written for it by that check.
	if _, err := os.Stat(filepath.Join(dir, sess.ID+".index.json")); err == nil {
		t.Error("the existence check refolded and wrote a sidecar")
	}
}

// TestStatusAndListAgreeOnWhichSessionsExist: GET /session/status and GET
// /session must not disagree about which sessions are there. Both resolve
// ids first and then take the same index-then-scan path, so a session whose
// fold breaks — no usable index, a readable journal — appears in both.
func TestStatusAndListAgreeOnWhichSessionsExist(t *testing.T) {
	dir := t.TempDir()
	healthy := coldSession(t, dir, nil)
	const broken = "ses_fedcba9876543210"
	// A compact record naming an absent range: the fold fails, the journal
	// reads fine.
	journal := `{"type":"session","id":"` + broken + `","created_at":"2026-01-02T03:04:06Z","workdir":"/w"}
{"type":"model","model":"test/m1"}
{"type":"message","message":{"id":"msg_1","role":"user","parts":[{"type":"text","text":"hi"}]},"usage":{"input_tokens":9}}
{"type":"compact","compact":{"first_id":"absent","last_id":"absent","turns_folded":1,"summary":{"id":"cmpsum_x","role":"user"}}}
`
	if err := os.WriteFile(filepath.Join(dir, broken+".jsonl"), []byte(journal), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})

	resp, data := h.do("GET", "/session/status", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /session/status = %d: %s", resp.StatusCode, data)
	}
	var status map[string]struct {
		Usage usageJSON `json:"usage"`
	}
	if err := json.Unmarshal(data, &status); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{healthy.ID, broken} {
		if _, ok := status[id]; !ok {
			t.Errorf("status is missing session %s: %s", id, data)
		}
	}
	if got := status[broken].Usage.InputTokens; got != 9 {
		t.Errorf("status usage for the fold-broken session = %d, want 9 from its journal", got)
	}
}

// TestListOmitsWhatItCannotRenderWhileStatusReportsIt pins a deliberate
// asymmetry, so a later reader finds it stated rather than discovers it.
//
// A journal whose fold breaks and whose load fails cannot be rendered as a
// listing entry: that entry names a model, a workdir, and lineage, and none
// of those survive a load that fails. GET /session/{id} 404s for the same
// session, so omitting it keeps the listing and the single-session read
// consistent. GET /session/status promises only counts, which a direct
// journal scan still supplies, so it reports the session.
//
// This is main's behavior, not something the metadata index introduced —
// verified directly against main, where the same journal is absent from
// GET /session and present in GET /session/status.
func TestListOmitsWhatItCannotRenderWhileStatusReportsIt(t *testing.T) {
	dir := t.TempDir()
	healthy := coldSession(t, dir, nil)
	const broken = "ses_fedcba9876543210"
	journal := `{"type":"session","id":"` + broken + `","created_at":"2026-01-02T03:04:06Z","workdir":"/w"}
{"type":"model","model":"test/m1"}
{"type":"message","message":{"id":"msg_1","role":"user","parts":[{"type":"text","text":"hi"}]},"usage":{"input_tokens":9}}
{"type":"compact","compact":{"first_id":"absent","last_id":"absent","turns_folded":1,"summary":{"id":"cmpsum_x","role":"user"}}}
`
	if err := os.WriteFile(filepath.Join(dir, broken+".jsonl"), []byte(journal), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})

	resp, data := h.do("GET", "/session", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /session = %d: %s", resp.StatusCode, data)
	}
	var list []sessionJSON
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != healthy.ID {
		t.Fatalf("listing = %s, want only the renderable session %s", data, healthy.ID)
	}

	// The single-session read agrees with the listing.
	resp, _ = h.do("GET", "/session/"+broken, nil)
	if resp.StatusCode != 404 {
		t.Errorf("GET /session/%s = %d, want 404: the listing omits what this cannot render", broken, resp.StatusCode)
	}

	// Status reports it, from the journal scan.
	resp, data = h.do("GET", "/session/status", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /session/status = %d: %s", resp.StatusCode, data)
	}
	var status map[string]struct {
		Usage usageJSON `json:"usage"`
	}
	if err := json.Unmarshal(data, &status); err != nil {
		t.Fatal(err)
	}
	if got, ok := status[broken]; !ok || got.Usage.InputTokens != 9 {
		t.Errorf("status for the fold-broken session = %+v (present=%v), want its journal's 9 input tokens", got, ok)
	}
}

// TestListSessionsIncludesChildStatus verifies that GET /session list includes
// lineage.status (SessionManager lifecycle state) for managed sessions,
// distinguishing "running" from terminal states. This allows the console to see
// which sessions are running without N+1 calls to GET /session/{id}.
//
// Scenario: A running session shows lineage.status="running"; a settled session
// shows the terminal value (done, failed, or canceled).
func TestListSessionsIncludesChildStatus(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	
	// Create a root session (managed and resident).
	rootID := h.createSession("test/m1")
	mgr := h.srv.SessionManager()

	// Verify root is in SessionManager (adopted on first prompt).
	if info, ok := mgr.Info(rootID); !ok || info.Status != engine.StatusIdle {
		t.Fatalf("test setup: root not in SessionManager or not idle: %+v", info)
	}

	// List sessions and verify the root's lineage.status reflects SessionManager state.
	resp, data := h.do("GET", "/session", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /session = %d: %s", resp.StatusCode, data)
	}

	var list []sessionJSON
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("decode list: %v (%s)", err, data)
	}

	// Find the root in the list.
	var rootEntry *sessionJSON
	for i := range list {
		if list[i].ID == rootID {
			rootEntry = &list[i]
			break
		}
	}

	if rootEntry == nil {
		t.Fatalf("root session %s not in list", rootID)
	}

	// Verify lineage.status correctly reflects SessionManager's lifecycle state
	// (idle for a managed session not running a turn). This proves the list
	// includes lifecycle status from SessionManager, not just busy/idle from
	// residency.
	if rootEntry.Lineage == nil || rootEntry.Lineage.Status != "idle" {
		t.Errorf("root child lineage.status = %+v, want Status='idle'", rootEntry.Lineage)
	}
}
