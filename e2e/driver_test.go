package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

type driver interface {
	Create(t *testing.T) string
	// CreateModel creates a session that names model and reports the response.
	CreateModel(t *testing.T, model string) callResult
	// PostInput admits an input with a new id and reports the receipt.
	PostInput(t *testing.T, id, text string) callResult
	// Repeat sends the newest input of the session again, under its id, with text as its body.
	Repeat(t *testing.T, id, text string, typed bool) callResult
	// SteerOtherTurn steers with an expected turn that is not running.
	SteerOtherTurn(t *testing.T, id, text string) callResult
	Models(t *testing.T) callResult
	// AwaitCommands returns once no typed command of the session is still running.
	AwaitCommands(t *testing.T, id string)
	// CommandRecords lists the newest status of each typed command of the session.
	CommandRecords(t *testing.T, id string) callResult
	Submit(t *testing.T, id, text string)
	// Attach submits text with the attachments after it.
	Attach(t *testing.T, id, text string, atts []attachment)
	Enqueue(t *testing.T, id, text string)
	// EnqueueNext enqueues text for the next turn. Serve has one queue, so it
	// is Enqueue there.
	EnqueueNext(t *testing.T, id, text string)
	WaitIdle(t *testing.T, id string)
	// AwaitTurnEnd returns once the log of the session holds the end of a turn. On serve, WaitIdle does not wait for it: a canceled session reads idle before its turn.ended is appended. It returns an error instead of failing the test, so a goroutine that the test starts may call it.
	AwaitTurnEnd(id string) error
	Interrupt(t *testing.T, id string)
	SetGoal(t *testing.T, id, condition string, maxTurns int, deferred bool)
	Messages(t *testing.T, id string) []transcriptMessage
	// Journals returns each event journal: one for the instance, or one
	// for each session where each session has its own seq.
	Journals(t *testing.T) [][]journalEntry
	Restart(t *testing.T, kill bool)
	Queued(t *testing.T, id string) []string
	AwaitGoalExhausted(t *testing.T)
	Stderr() string
	Workdir() string

	Compact(t *testing.T, id string) callResult
	SetModel(t *testing.T, id, model string) callResult
	SetThinking(t *testing.T, id, level string) callResult
	SetServiceTier(t *testing.T, id, tier string) callResult
	EndSession(t *testing.T, id string) callResult
	Send(t *testing.T, id, text string) callResult
	CancelTree(t *testing.T, id string) callResult
	DeleteQueued(t *testing.T, id string) callResult
	UpdateGoal(t *testing.T, id, condition string) callResult
	ClearGoal(t *testing.T, id string) callResult
	ListSessions(t *testing.T) callResult
	GetSession(t *testing.T, id string) callResult
	SessionStatus(t *testing.T) callResult
	MessagesPage(t *testing.T, id string, beforeSeq, limit int) callResult
	Bootstrap(t *testing.T, id string, limit int) callResult
	JournalPage(t *testing.T, id string, from, limit int) callResult
	SSEResume(t *testing.T, id string, afterSeq int64, header, scoped bool) callResult
	Child(t *testing.T, parentID string, nth int) string
	// AwaitChildSettled returns once the log of the parent records that the child settled.
	AwaitChildSettled(t *testing.T, parentID, childID string)
	Command(t *testing.T, id, text string, repeatable bool) callResult
	Commands(t *testing.T) callResult
	Processes(t *testing.T) callResult
	ProcessAction(t *testing.T, name, action string) callResult
	ProcessLogs(t *testing.T, name string, tail int) callResult
	WorkspaceChanges(t *testing.T, scope, dir string) callResult
}

// attachment is a file of a prompt.
type attachment struct {
	mediaType string
	data      []byte
}

// callResult is what a driver reports for a call: the status and the decoded
// body. A body that carries a transcript reports it in Messages, in the
// oracle's own vocabulary, and leaves the key out of Body.
type callResult struct {
	Status   int
	Body     any
	Messages []transcriptMessage
	// Wire is the raw reply of a route whose rows check the bytes as well as
	// the decoded body. It is not recorded.
	Wire []byte
}

// waitBound is a failure bound for a wait on the serve process, not a delay.
// It is far above any real latency; a wait that reaches it fails the test.
var waitBound = 60 * time.Second

// waitMargin is the time a request may run past waitBound for the server to
// honor a timeout_s of waitBound and answer.
var waitMargin = 10 * time.Second

// scenarioConfig is the served config of a scenario: extra over the defaults.
func scenarioConfig(extra map[string]any) map[string]any {
	cfg := map[string]any{"context_window_tokens": 1_000_000} // the modelmeta table is bot-refreshed
	maps.Copy(cfg, extra)
	return cfg
}

// rawBody is a request body that goes on the wire as written, with the
// whitespace that json.Marshal would remove.
type rawBody string

func encodeBody(body any) ([]byte, error) {
	if raw, ok := body.(rawBody); ok {
		return []byte(raw), nil
	}
	return json.Marshal(body)
}

func withQuery(path string, kv ...any) string {
	q := url.Values{}
	for i := 0; i < len(kv); i += 2 {
		if n := kv[i+1].(int); n != 0 {
			q.Set(kv[i].(string), strconv.Itoa(n))
		}
	}
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

func TestWaitsFailAtTheirBound(t *testing.T) {
	skipShort(t)
	old := waitBound
	oldMargin := waitMargin
	waitBound, waitMargin = 100*time.Millisecond, 0
	t.Cleanup(func() { waitBound, waitMargin = old, oldMargin })

	t.Run("request", func(t *testing.T) {
		stuck := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		t.Cleanup(stuck.Close)
		p := &serveProc{t: t, addr: strings.TrimPrefix(stuck.URL, "http://")}
		if _, _, err := p.send(http.MethodGet, "/health", nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("send to a server that never answers = %v, want context deadline exceeded", err)
		}
	})
}
