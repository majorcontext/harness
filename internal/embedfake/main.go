// Command embedfake serves the scripted Anthropic server of harnesstest for
// the node tests of sdk/embed. The script is a JSON array in the first
// argument: [{"text": "...", "toolCalls": [{"id", "name", "input"}], "block": false}].
// It prints the base URL on one line. GET /__requests on that URL returns
// every request as [{"path", "headers", "body"}]. The process ends on SIGTERM.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

type scriptedReply struct {
	Text      string `json:"text"`
	Block     bool   `json:"block"`
	ToolCalls []struct {
		ID    string         `json:"id"`
		Name  string         `json:"name"`
		Input map[string]any `json:"input"`
	} `json:"toolCalls"`
}

type recorded struct {
	Path    string              `json:"path"`
	Headers map[string][]string `json:"headers"`
	Body    json.RawMessage     `json:"body"`
}

// scriptTB lets the scripted model server run outside a test. It implements
// only the methods that harnesstest calls on its start-up path.
type scriptTB struct{ testing.TB }

func (scriptTB) Helper()               {}
func (scriptTB) Failed() bool          { return false }
func (scriptTB) Cleanup(func())        {}
func (scriptTB) Errorf(string, ...any) {}
func (scriptTB) Fatalf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "embedfake: "+f+"\n", a...)
	os.Exit(1)
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: embedfake '<script json>'")
		os.Exit(2)
	}
	var script []scriptedReply
	if err := json.Unmarshal([]byte(os.Args[1]), &script); err != nil {
		fmt.Fprintln(os.Stderr, "embedfake:", err)
		os.Exit(2)
	}
	steps := make([]harnesstest.Step, len(script))
	for i, r := range script {
		reply := harnesstest.Reply{Text: r.Text, Block: r.Block}
		for _, c := range r.ToolCalls {
			reply.ToolCalls = append(reply.ToolCalls, harnesstest.ToolCall{ID: c.ID, Name: c.Name, Input: c.Input})
		}
		steps[i] = harnesstest.Step{Name: "step" + strconv.Itoa(i), Reply: reply}
	}
	model := harnesstest.NewAnthropic(scriptTB{}, steps...)
	target, err := url.Parse(model.URL())
	if err != nil {
		fmt.Fprintln(os.Stderr, "embedfake:", err)
		os.Exit(1)
	}
	front := httptest.NewServer(recorder(target))
	fmt.Println(front.URL)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	<-stop
	front.Close()
}

// recorder forwards each request to the scripted server and keeps a copy of
// its path, headers, and body.
func recorder(target *url.URL) http.Handler {
	var mu sync.Mutex
	var seen []recorded
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__requests" {
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewEncoder(w).Encode(seen)
			return
		}
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		raw := json.RawMessage(body)
		if !json.Valid(body) {
			raw = json.RawMessage("null")
		}
		mu.Lock()
		seen = append(seen, recorded{Path: r.URL.Path, Headers: r.Header.Clone(), Body: raw})
		mu.Unlock()
		proxy.ServeHTTP(w, r)
	})
}
