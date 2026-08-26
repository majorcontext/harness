package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// failStreamProvider fails every Stream call with err — here, the
// account-level wall the anthropic adapter classifies
// (provider.ErrKindProviderExhausted).
type failStreamProvider struct {
	name string
	err  error
}

func (p *failStreamProvider) Name() string { return p.name }

func (p *failStreamProvider) Stream(context.Context, *provider.Request) (provider.Stream, error) {
	return nil, p.err
}

// TestLineageReportsProviderExhaustedFailKind proves the structured child
// outcome reaches the wire: a control plane polling GET /session/{child}
// learns the ACCOUNT is walled — so the child is intact and re-runnable,
// and a replacement child would hit the same wall — without parsing the
// fail_reason prose.
func TestLineageReportsProviderExhaustedFailKind(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		rootProv := &scriptedProvider{name: "root", turns: [][]provider.Event{
			toolCallTurn("tc1", "task", `{"agent":"general-purpose","prompt":"find the answer","model":"child/m1"}`),
			asstTurn("spawned it"),
			asstTurn("noted"), // absorbs the completion-notification resume
		}}
		childProv := &failStreamProvider{name: "child", err: provider.MarkPermanent(&provider.Error{
			Kind:        provider.ErrKindProviderExhausted,
			Raw:         "anthropic: You have reached your specified API usage limits. You will regain access on 2026-09-01. (invalid_request_error, HTTP 400)",
			RecoverHint: "2026-09-01",
		})}
		reg := provider.Registry{rootProv.Name(): rootProv, childProv.Name(): childProv}

		var srv *Server
		opts := Options{
			SessionDir: dir,
			RunToken:   "secret-run-token",
			Version:    "9.9.9",
			NewSession: func(m message.ModelRef, workDir, parentSession string) (*engine.Session, error) {
				return engine.NewSession(engine.Config{
					Providers:      reg,
					Model:          m,
					WorkDir:        workDir,
					ParentSession:  parentSession,
					SessionDir:     dir,
					OnEvent:        func(ev engine.Event) { srv.Publish(ev) },
					SessionManager: srv.sessMgr,
				}), nil
			},
			LoadSession: func(id string) (*engine.Session, error) {
				return engine.LoadSession(engine.Config{
					Providers:      reg,
					SessionDir:     dir,
					OnEvent:        func(ev engine.Event) { srv.Publish(ev) },
					SessionManager: srv.sessMgr,
				}, id)
			},
		}
		var err error
		srv, err = New(opts)
		if err != nil {
			t.Fatal(err)
		}

		rootID := createSessionDirect(t, srv, "root/m1")
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/session/"+rootID+"/prompt_async",
			strings.NewReader(`{"parts":[{"type":"text","text":"go"}]}`))
		req.SetPathValue("id", rootID)
		srv.handlePrompt(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("prompt_async status %d: %s", rec.Code, rec.Body)
		}
		synctest.Wait()

		info, ok := srv.sessMgr.Info(rootID)
		if !ok || len(info.Children) != 1 {
			t.Fatalf("root lineage children = %v, want exactly 1 spawned child", info.Children)
		}
		childID := info.Children[0]

		getRec := httptest.NewRecorder()
		getReq := httptest.NewRequest("GET", "/session/"+childID, nil)
		getReq.SetPathValue("id", childID)
		srv.handleGet(getRec, getReq)
		if getRec.Code != http.StatusOK {
			t.Fatalf("GET /session/%s status %d: %s", childID, getRec.Code, getRec.Body)
		}
		var got struct {
			Lineage struct {
				Status     string `json:"status"`
				FailKind   string `json:"fail_kind"`
				FailReason string `json:"fail_reason"`
			} `json:"lineage"`
		}
		if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal: %v (%s)", err, getRec.Body)
		}
		if got.Lineage.Status != string(engine.StatusFailed) {
			t.Errorf("lineage.status = %q, want %q", got.Lineage.Status, engine.StatusFailed)
		}
		if got.Lineage.FailKind != engine.FailKindProviderExhausted {
			t.Errorf("lineage.fail_kind = %q, want %q", got.Lineage.FailKind, engine.FailKindProviderExhausted)
		}
		if !strings.Contains(got.Lineage.FailReason, "2026-09-01") {
			t.Errorf("lineage.fail_reason = %q, want the recover-at hint", got.Lineage.FailReason)
		}
	})
}
