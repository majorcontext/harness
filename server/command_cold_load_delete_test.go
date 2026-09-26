package server

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
)

func TestHandleEndRefusesDuringColdCommandLoad(t *testing.T) {
	h := newHarnessOpts(t, t.TempDir(), newCapturingProvider(), 1)
	id := h.createSession("test/m1")
	h.createSession("test/m1")
	h.srv.mu.Lock()
	_, resident := h.srv.sessions[id]
	h.srv.mu.Unlock()
	if resident {
		t.Fatal("test setup: command session is resident")
	}

	load := h.srv.opts.LoadSession
	loadStarted := make(chan struct{})
	allowLoad := make(chan struct{})
	defer func() {
		select {
		case <-allowLoad:
		default:
			close(allowLoad)
		}
	}()
	h.srv.opts.LoadSession = func(loadID string) (*engine.Session, error) {
		if loadID == id {
			close(loadStarted)
			<-allowLoad
		}
		return load(loadID)
	}

	sse := h.openSSE("?from=0", "")
	type response struct {
		resp *http.Response
		data []byte
	}
	commandDone := make(chan response, 1)
	go func() {
		resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
			"parts":  []map[string]string{{"type": "text", "text": "/status"}},
			"source": "typed",
		})
		commandDone <- response{resp: resp, data: data}
	}()
	<-loadStarted

	beforeSeq := h.srv.currentSeq()
	resp, data := h.do("DELETE", "/session/"+id, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("DELETE during cold command load status = %d: %s, want 409", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "session is running a command; retry after it finishes") {
		t.Fatalf("DELETE conflict response = %s, want retry message", data)
	}
	if got := h.srv.currentSeq(); got != beforeSeq {
		t.Fatalf("journal sequence after rejected DELETE = %d, want unchanged %d", got, beforeSeq)
	}

	close(allowLoad)
	cmdResp := <-commandDone
	if cmdResp.resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async status = %d: %s, want 202", cmdResp.resp.StatusCode, cmdResp.data)
	}
	accepted := sse.waitFor(t, "command")
	if accepted.Command == nil || accepted.Command.Status != message.CommandAccepted {
		t.Fatalf("accepted command = %+v, want accepted", accepted.Command)
	}
	terminal := sse.waitFor(t, "command")
	if terminal.Command == nil || terminal.Command.Status != message.CommandSucceeded {
		t.Fatalf("terminal command = %+v, want succeeded", terminal.Command)
	}

	h.srv.mu.Lock()
	sess := h.srv.sessions[id].sess
	h.srv.mu.Unlock()
	commands := sess.Commands()
	if len(commands) != 1 || commands[0].Status != message.CommandSucceeded {
		t.Fatalf("Commands() = %+v, want one succeeded command", commands)
	}
	resp, data = h.do("DELETE", "/session/"+id, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE after command status = %d: %s, want 204", resp.StatusCode, data)
	}
}

func TestMutableSessionLoadFailureReleasesDeleteReservation(t *testing.T) {
	h := newHarnessOpts(t, t.TempDir(), newCapturingProvider(), 1)
	id := h.createSession("test/m1")
	h.createSession("test/m1")

	load := h.srv.opts.LoadSession
	loadStarted := make(chan struct{})
	allowLoad := make(chan struct{})
	defer func() {
		select {
		case <-allowLoad:
		default:
			close(allowLoad)
		}
	}()
	h.srv.opts.LoadSession = func(loadID string) (*engine.Session, error) {
		if loadID == id {
			close(loadStarted)
			<-allowLoad
			return nil, errors.New("load failed")
		}
		return load(loadID)
	}
	commandDone := make(chan int, 1)
	go func() {
		resp, _ := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
			"parts":  []map[string]string{{"type": "text", "text": "/status"}},
			"source": "typed",
		})
		commandDone <- resp.StatusCode
	}()
	<-loadStarted
	resp, data := h.do("DELETE", "/session/"+id, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("DELETE during failed load status = %d: %s, want 409", resp.StatusCode, data)
	}
	close(allowLoad)
	if status := <-commandDone; status != http.StatusNotFound {
		t.Fatalf("prompt_async after load failure status = %d, want 404", status)
	}

	resp, data = h.do("DELETE", "/session/"+id, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE after load failure status = %d: %s, want 204", resp.StatusCode, data)
	}
}
