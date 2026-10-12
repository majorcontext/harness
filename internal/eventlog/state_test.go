package eventlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/protocol"
)

var t0 = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func created() protocol.Payload { return protocol.SessionCreated{Model: "openai/gpt-5", Origin: "cli"} }

func admit(id string) protocol.Payload {
	return protocol.InputAdmitted{InputID: id, Delivery: protocol.DeliveryQueue, Source: "user", Parts: []protocol.LogPart{{Type: protocol.LogPartText, Text: "hi"}}}
}

func steer(id string) protocol.Payload {
	return protocol.InputAdmitted{InputID: id, Delivery: protocol.DeliverySteer, Source: "user", Parts: []protocol.LogPart{{Type: protocol.LogPartText, Text: "hi"}}}
}

func start(turn string, inputs ...string) protocol.Payload {
	return protocol.TurnStarted{TurnID: turn, InputIDs: inputs}
}
func promote(in, turn string) protocol.Payload {
	return protocol.InputPromoted{InputID: in, TurnID: turn}
}
func withdraw(in string) protocol.Payload { return protocol.InputWithdrawn{InputID: in} }

func call(turn, item string, ids ...string) protocol.Payload {
	m := protocol.LogMessage{Role: protocol.RoleAssistant}
	for _, id := range ids {
		m.Parts = append(m.Parts, protocol.LogPart{Type: protocol.LogPartToolCall, CallID: id, Name: "bash", Arguments: json.RawMessage(`{"cmd":"ls"}`)})
	}
	return protocol.ItemCompleted{ItemID: item, TurnID: turn, Message: m}
}

func result(turn, item, id string) protocol.Payload {
	return protocol.ItemCompleted{ItemID: item, TurnID: turn, Message: protocol.LogMessage{Role: protocol.RoleTool, Parts: []protocol.LogPart{
		{Type: protocol.LogPartToolResult, CallID: id, Text: "ok"}}}}
}

func resultBlob(turn, item, id, key string) protocol.Payload {
	return protocol.ItemCompleted{ItemID: item, TurnID: turn, Message: protocol.LogMessage{Role: protocol.RoleTool, Parts: []protocol.LogPart{
		{Type: protocol.LogPartToolResult, CallID: id, Text: "ok", Blobs: []protocol.LogPart{{Type: protocol.LogPartBlob, MediaType: "image/png", BlobKey: key, Bytes: 4}}}}}}
}

func end(turn string, r protocol.StopReason, cause protocol.Cause) protocol.Payload {
	return protocol.TurnEnded{TurnID: turn, StopReason: r, Cause: cause}
}

func ask(req, item string) protocol.Payload {
	return protocol.RequestOpened{RequestID: req, ItemID: item, RequestKind: "question", Payload: json.RawMessage(`{"q":"which?"}`)}
}

func resolve(req string) protocol.Payload {
	return protocol.RequestResolved{RequestID: req, Resolution: protocol.ResolutionAnswered, Answer: json.RawMessage(`"yes"`)}
}

func dismiss(req string) protocol.Payload {
	return protocol.RequestResolved{RequestID: req, Resolution: protocol.ResolutionDismissed}
}
func cmd(id, status string) protocol.Payload {
	return protocol.CommandRecorded{InputID: id, Line: "/x", Name: "x", Status: status}
}
func suspend(turn string, c protocol.Cause) protocol.Payload {
	return protocol.TurnSuspended{TurnID: turn, Cause: c}
}
func resume(turn string, n int) protocol.Payload { return protocol.TurnResumed{TurnID: turn, Count: n} }
func goal(s protocol.GoalState) protocol.Payload { return protocol.GoalChanged{State: s} }
func verdict(turn string) protocol.Payload {
	return protocol.GoalEvaluated{TurnID: turn, Verdict: protocol.VerdictNotMet}
}

var setGoal protocol.Payload = protocol.GoalSet{Condition: "tests pass", MaxTurns: 3}

func with(base []protocol.Payload, more ...protocol.Payload) []protocol.Payload {
	return append(append([]protocol.Payload(nil), base...), more...)
}

var (
	base      = []protocol.Payload{created()}
	running   = with(base, admit("a"), start("t1", "a"))
	calling   = with(running, call("t1", "i1", "c1"))
	asking    = with(calling, ask("c1", "i1"))
	suspended = with(calling, result("t1", "i2", "c1"), suspend("t1", protocol.CauseHandoff))
	evaluated = with(base, setGoal, admit("a"), start("t1", "a"), end("t1", protocol.StopCompleted, ""), verdict("t1"))
)

type view struct {
	Status   protocol.Status
	Turn     string
	Queue    []string
	Requests []string
	Calls    []string
	Goal     string
}

func viewOf(s *State) view {
	v := view{Status: s.Status()}
	if t, ok := s.Turn(); ok {
		v.Turn = t.ID
		if t.Suspended {
			v.Turn += " suspended"
		}
	}
	for _, in := range s.Queue() {
		v.Queue = append(v.Queue, in.InputID)
	}
	for _, r := range s.Requests() {
		v.Requests = append(v.Requests, r.RequestID)
	}
	for _, c := range s.OpenToolCalls() {
		v.Calls = append(v.Calls, c.CallID)
	}
	if g, ok := s.Goal(); ok {
		v.Goal = fmt.Sprintf("%s %d", g.State, g.Turns)
	}
	return v
}

func record(t *testing.T, seq uint64, e protocol.Payload) Record {
	t.Helper()
	data, err := Envelope{Seq: seq, Time: t0, Event: e}.Encode()
	if err != nil {
		t.Fatalf("encode %s: %v", e.Kind(), err)
	}
	return Record{Seq: seq, Data: data}
}

func replay(t *testing.T, events []protocol.Payload) *State {
	t.Helper()
	var s State
	for _, e := range events {
		if err := s.Apply(record(t, s.Head()+1, e)); err != nil {
			t.Fatalf("apply %s: %v", e.Kind(), err)
		}
	}
	return &s
}

var applyRows = []struct {
	name   string
	events []protocol.Payload
	err    string
	want   view
}{
	{"an admitted input waits in the queue", with(base, admit("a")), "", view{Status: protocol.StatusIdle, Queue: []string{"a"}}},
	{"a record before session.created fails", []protocol.Payload{admit("a")}, "before session.created", view{}},
	{"session.created comes once", with(base, created()), "session already created", view{}},
	{"a turn takes its queued input", running, "", view{Status: protocol.StatusRunning, Turn: "t1"}},
	{"a steer input joins the running turn", with(running, steer("s"), promote("s", "t1")), "", view{Status: protocol.StatusRunning, Turn: "t1"}},
	{"a withdrawn input leaves the queue", with(base, admit("a"), withdraw("a")), "", view{Status: protocol.StatusIdle}},
	{"a promoted input is not withdrawn", with(running, withdraw("a")), "input a is promoted", view{}},
	{"an input drives one turn", with(running, end("t1", protocol.StopCompleted, ""), start("t2", "a")), "input a is promoted", view{}},
	{"a queue input waits for the next turn", with(running, admit("b"), promote("b", "t1")), "input b waits for the next turn", view{}},
	{"a withdrawn input is not promoted", with(base, admit("a"), withdraw("a"), admit("b"), start("t1", "b"), promote("a", "t1")), "input a is withdrawn", view{}},
	{"an input id is admitted once", with(base, admit("a"), admit("a")), "input a is admitted", view{}},
	{"a promotion needs its turn running", with(base, admit("a"), admit("b"), start("t1", "a"), promote("b", "t2")), "turn t2 is not running", view{}},
	{"a turn starts only with no current turn", with(running, admit("b"), start("t2", "b")), "turn t1 is current", view{}},
	{"a turn id is used once", with(running, end("t1", protocol.StopCompleted, ""), admit("b"), start("t1", "b")), "turn t1 was used", view{}},
	{"a turn starts only from queued inputs", with(base, start("t1", "x")), "input x is unknown", view{}},
	{"an item needs its turn running", with(running, call("t2", "i1", "c1")), "turn t2 is not running", view{}},
	{"a tool result closes its call", with(calling, result("t1", "i2", "c1"), end("t1", protocol.StopCompleted, "")), "", view{Status: protocol.StatusIdle}},
	{"a tool result holds blob parts with a key", with(calling, resultBlob("t1", "i2", "c1", "toolblob-1"), end("t1", protocol.StopCompleted, "")), "", view{Status: protocol.StatusIdle}},
	{"a tool result blob needs a key", with(calling, resultBlob("t1", "i2", "c1", "")), "tool result c1 has a blob 0 that is not a blob part with a blob_key", view{}},
	{"a tool call gets one result", with(calling, result("t1", "i2", "c1"), result("t1", "i3", "c1")), "no open tool call c1", view{}},
	{"a tool call id is open once", with(calling, call("t1", "i2", "c1")), "tool call c1 is open", view{}},
	{"a turn does not end with an unanswered tool call", with(calling, end("t1", protocol.StopCompleted, "")), "tool call c1 has no result", view{}},
	{"a request holds a tool call open past the turn", with(asking, end("t1", protocol.StopAwaitingInput, "")), "", view{Status: protocol.StatusWaiting, Requests: []string{"c1"}, Calls: []string{"c1"}}},
	{"a request holds open only the call that it names", with(running, call("t1", "i1", "c1", "c2"), ask("c1", "i1"), end("t1", protocol.StopAwaitingInput, "")), "tool call c2 has no result", view{}},
	{"awaiting_input needs an open request", with(running, end("t1", protocol.StopAwaitingInput, "")), "no open request", view{}},
	{"awaiting_input needs a request from its own turn", with(asking, result("t1", "i2", "c1"), end("t1", protocol.StopCompleted, ""), start("t2"), end("t2", protocol.StopAwaitingInput, "")), "turn t2 awaits input with no open request from this turn", view{}},
	{"a turn does not start while a tool call is open", with(asking, end("t1", protocol.StopAwaitingInput, ""), start("t2")), "turn t2 starts with open tool call c1", view{}},
	{"an input is not admitted while a request is open", with(asking, admit("b")), "request c1 is open", view{}},
	{"a dismissed request lets an input start a turn", with(asking, end("t1", protocol.StopAwaitingInput, ""), dismiss("c1"), admit("b"), start("t2", "b")), "", view{Status: protocol.StatusRunning, Turn: "t2"}},
	{"an answer leaves the tool call open for the turn that it starts", with(asking, end("t1", protocol.StopAwaitingInput, ""), resolve("c1")), "", view{Status: protocol.StatusIdle, Calls: []string{"c1"}}},
	{"a turn starts while the call of an answer is open", with(asking, end("t1", protocol.StopAwaitingInput, ""), resolve("c1"), start("t2")), "", view{Status: protocol.StatusRunning, Turn: "t2", Calls: []string{"c1"}}},
	{"a result closes the call of an answer", with(asking, end("t1", protocol.StopAwaitingInput, ""), resolve("c1"), start("t2"), result("t2", "i2", "c1"), end("t2", protocol.StopCompleted, "")), "", view{Status: protocol.StatusIdle}},
	{"a turn does not end with the call of an answer unresulted", with(asking, end("t1", protocol.StopAwaitingInput, ""), resolve("c1"), start("t2"), end("t2", protocol.StopCompleted, "")), "tool call c1 has no result", view{}},
	{"a dismissal closes the tool call of its request", with(asking, end("t1", protocol.StopAwaitingInput, ""), dismiss("c1")), "", view{Status: protocol.StatusIdle}},
	{"a dismissal closes only the call that it names", with(running, call("t1", "i1", "c1", "c2"), ask("c1", "i1"), dismiss("c1")), "", view{Status: protocol.StatusRunning, Turn: "t1", Calls: []string{"c2"}}},
	{"a request opens on an open tool call named by its id", with(running, call("t1", "i1", "c1"), ask("c9", "i1")), "no open tool call c9", view{}},
	{"a request resolves once", with(asking, resolve("c1"), resolve("c1")), "request c1 is not open", view{}},
	{"a request opens only in a running turn", with(base, ask("c1", "i1")), "no running turn", view{}},
	{"a handoff suspends the turn", suspended, "", view{Status: protocol.StatusIdle, Turn: "t1 suspended"}},
	{"a suspended turn resumes", with(suspended, resume("t1", 1)), "", view{Status: protocol.StatusRunning, Turn: "t1"}},
	{"only a handoff suspends a turn", with(running, suspend("t1", protocol.CauseStopped)), "cause stopped", view{}},
	{"a turn suspends with no open tool call", with(calling, suspend("t1", protocol.CauseHandoff)), "turn t1 suspends with open tool call c1", view{}},
	{"an open request does not let a turn suspend", with(asking, suspend("t1", protocol.CauseHandoff)), "turn t1 suspends with open tool call c1", view{}},
	{"a resume counts each resume", with(suspended, resume("t1", 2)), "count 2, want 1", view{}},
	{"a stopped turn never resumes", with(running, end("t1", protocol.StopInterrupted, protocol.CauseStopped), resume("t1", 1)), "turn t1 is not suspended", view{}},
	{"a suspended turn does not end", with(suspended, end("t1", protocol.StopCompleted, "")), "turn t1 is suspended", view{}},
	{"a failed turn takes no interrupt cause", with(running, end("t1", protocol.StopFailed, protocol.CauseStopped)), "failure cause", view{}},
	{"a completed turn has no cause", with(running, end("t1", protocol.StopCompleted, protocol.CauseStopped)), "completes with cause", view{}},
	{"an interrupted turn names its cause", with(running, end("t1", protocol.StopInterrupted, "")), "interrupt cause", view{}},
	{"a goal counts evaluated turns", evaluated, "", view{Status: protocol.StatusIdle, Goal: "active 1"}},
	{"a turn is evaluated once", with(evaluated, verdict("t1")), "turn t1 was evaluated", view{}},
	{"a new goal resets the count", with(evaluated, setGoal), "", view{Status: protocol.StatusIdle, Goal: "active 0"}},
	{"a paused goal becomes active", with(base, setGoal, goal(protocol.GoalPaused), goal(protocol.GoalActive)), "", view{Status: protocol.StatusIdle, Goal: "active 0"}},
	{"an achieved goal stays achieved", with(base, setGoal, goal(protocol.GoalAchieved), goal(protocol.GoalActive)), "goal achieved cannot become active", view{}},
	{"any goal clears", with(base, setGoal, goal(protocol.GoalExhausted), goal(protocol.GoalCleared)), "", view{Status: protocol.StatusIdle, Goal: "cleared 0"}},
	{"a cleared goal does not clear again", with(base, setGoal, goal(protocol.GoalCleared), goal(protocol.GoalCleared)), "goal cleared cannot become cleared", view{}},
	{"a new goal does not evaluate an ended turn", with(base, admit("a"), start("t1", "a"), end("t1", protocol.StopCompleted, ""), setGoal, verdict("t1")), "turn t1 was evaluated", view{}},
	{"a goal evaluates no more turns than max_turns", with(base, protocol.GoalSet{Condition: "x", MaxTurns: 1}, admit("a"), start("t1", "a"), end("t1", protocol.StopCompleted, ""), verdict("t1"), admit("b"), start("t2", "b"), end("t2", protocol.StopCompleted, ""), verdict("t2")), "max_turns 1 is reached", view{}},
	{"an evaluation names the last ended turn", with(running, setGoal, verdict("t1")), "turn t1 is not the last ended turn", view{}},
	{"an evaluation needs an active goal", with(running, end("t1", protocol.StopCompleted, ""), verdict("t1")), "no active goal", view{}},
	{"a compaction covers only earlier records", with(base, protocol.CompactionApplied{FromSeq: 1, ToSeq: 2}), "to_seq 2", view{}},
	{"a child settles after it spawns", with(base, protocol.ChildSettled{ChildID: "x", Outcome: protocol.OutcomeDone}), "child x is unknown", view{}},
	{"an adjusted goal keeps its turn count", with(evaluated, protocol.GoalSet{Condition: "y", MaxTurns: 3, Turns: 1}), "", view{Status: protocol.StatusIdle, Goal: "active 1"}},
	{"a goal starts below max_turns", with(base, protocol.GoalSet{Condition: "x", MaxTurns: 1, Turns: 1}), "turns", view{}},
	{"a settled child spawns again", with(base, protocol.ChildSpawned{ChildID: "x"}, protocol.ChildSettled{ChildID: "x", Outcome: protocol.OutcomeDone}, protocol.ChildSpawned{ChildID: "x"}), "", view{Status: protocol.StatusIdle}},
	{"an unsettled child does not spawn again", with(base, protocol.ChildSpawned{ChildID: "x"}, protocol.ChildSpawned{ChildID: "x"}), "already spawned", view{}},
	{"a settings change keeps a model", with(base, protocol.SettingsChanged{Model: new("")}), "empty model", view{}},
	{"an accepted command ends once", with(base, cmd("a", "accepted"), cmd("a", "succeeded")), "", view{Status: protocol.StatusIdle}},
	{"an ended command records nothing more", with(base, cmd("a", "accepted"), cmd("a", "failed"), cmd("a", "failed")), "command a is failed", view{}},
	{"a command is accepted once", with(base, cmd("a", "accepted"), cmd("a", "accepted")), "command a is accepted", view{}},
	{"an input ID is not a command", with(base, admit("a"), cmd("a", "failed")), "command a is an input", view{}},
	{"a command ID is not an input", with(base, cmd("a", "failed"), admit("a")), "input a is a command", view{}},
}

func TestApply(t *testing.T) {
	for _, tc := range applyRows {
		t.Run(tc.name, func(t *testing.T) {
			last := len(tc.events) - 1
			s := replay(t, tc.events[:last])
			checkErr := Check(s, tc.events[last:])
			err := s.Apply(record(t, s.Head()+1, tc.events[last]))
			if tc.err == "" {
				if err != nil || checkErr != nil {
					t.Fatalf("Apply = %v, Check = %v; want nil", err, checkErr)
				}
				if got := viewOf(s); !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("view = %+v, want %+v", got, tc.want)
				}
				return
			}
			if !errors.Is(err, ErrIllegal) || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("Apply = %v, want ErrIllegal with %q", err, tc.err)
			}
			if checkErr == nil || checkErr.Error() != err.Error() {
				t.Fatalf("Check = %v, want %v", checkErr, err)
			}
			if !reflect.DeepEqual(s, replay(t, tc.events[:last])) {
				t.Fatal("a failed Apply changed the state")
			}
		})
	}
}

func TestCheckRejectsNilEvent(t *testing.T) {
	if err := Check(&State{}, []protocol.Payload{nil}); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("Check = %v, want ErrUnknownKind", err)
	}
}

func TestCheckMatchesAppend(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event protocol.Payload
		err   error
	}{
		{"a pointer event passes as its value", &protocol.InputAdmitted{InputID: "b", Delivery: protocol.DeliveryQueue}, nil},
		{"a malformed payload fails as it would at append", protocol.RequestOpened{RequestID: "r", ItemID: "i1", Payload: json.RawMessage(`{bad`)}, errAny},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Check(replay(t, calling), []protocol.Payload{tc.event})
			if (err != nil) != (tc.err != nil) {
				t.Fatalf("Check = %v, want error %v", err, tc.err)
			}
		})
	}
}

var errAny = errors.New("any error")

func TestAccessorsDoNotAliasState(t *testing.T) {
	events := with(base, admit("a"), start("t1", "a"), admit("b"), call("t1", "i1", "c1"), ask("c1", "i1"))
	s := replay(t, events)
	turn, _ := s.Turn()
	turn.InputIDs[0] = "x"
	q := s.Queue()
	q[0].Parts[0].Text = "x"
	q[0].Parts = append(q[0].Parts, protocol.LogPart{})
	s.Requests()[0].Payload[0] = 'x'
	in, _, _ := s.Input("b")
	in.Parts[0].Text = "x"
	s.History()[1].Parts[0].Arguments[0] = 'x'
	if !reflect.DeepEqual(s, replay(t, events)) {
		t.Fatal("a caller changed the state through an accessor")
	}
	folded := with(base, says("a", protocol.DeliveryQueue, "one"), start("t1", "a"), call("t1", "i1", "c1"), result("t1", "i2", "c1"), end("t1", protocol.StopCompleted, ""))
	folded = with(folded, says("b", protocol.DeliveryQueue, "two"), start("t2", "b"), protocol.CompactionApplied{FromSeq: 1, ToSeq: 6, Summary: "sum"})
	s = replay(t, folded)
	page := s.MessagePage(0, 10)
	if len(page.Messages) < 3 || page.Messages[0].ID != "msg_a" {
		t.Fatalf("page after a compaction = %+v, want the folded messages first", page.Messages)
	}
	for i := range page.Messages {
		page.Messages[i].Parts[0].Text = "x"
		page.Messages[i].OperatorBatch = append(page.Messages[i].OperatorBatch, protocol.OperatorBatchEntry{})
	}
	page.Messages[1].Parts[0].Arguments[0] = 'x'
	if !reflect.DeepEqual(s.MessagePage(0, 10), replay(t, folded).MessagePage(0, 10)) {
		t.Fatal("a caller changed the state through a message page")
	}
}

func TestRetainedIsACopy(t *testing.T) {
	s := replay(t, []protocol.Payload{created(), protocol.ToolResultRetained{Handle: "trh_1", Tool: "bash", BlobKey: "k"}})
	s.Retained()[0].Handle = "x"
	if got := s.Retained()[0].Handle; got != "trh_1" {
		t.Errorf("handle after a caller write = %q, want trh_1", got)
	}
}

func TestRecordedUsageFoldsIntoTheState(t *testing.T) {
	first := &protocol.SubscriptionUsage{Provider: "codex", Plan: "plus", CapturedAt: 1, Windows: []protocol.SubscriptionUsageWindow{{Key: "primary", UsedPercent: 12}}}
	second := &protocol.SubscriptionUsage{Provider: "codex", Plan: "plus", CapturedAt: 2, Windows: []protocol.SubscriptionUsageWindow{{Key: "primary", UsedPercent: 15}}}
	s := replay(t, with(base, setGoal, admit("a"), start("t1", "a"),
		protocol.ContextMeasured{Tokens: 100, Window: 1000, Source: "m", Usage: protocol.Usage{InputTokens: 10, OutputTokens: 2}, SubscriptionUsage: first},
		protocol.ContextMeasured{Usage: protocol.Usage{InputTokens: 5, OutputTokens: 1}, SubscriptionUsage: second},
		protocol.ContextMeasured{Usage: protocol.Usage{OutputTokens: 1}},
		protocol.TurnEnded{TurnID: "t1", StopReason: protocol.StopFailed, Error: "boom"},
		protocol.CompactionApplied{FromSeq: 1, ToSeq: 3, Summary: "s", Usage: protocol.Usage{InputTokens: 7, OutputTokens: 3}},
		protocol.GoalEvaluated{TurnID: "t1", Verdict: protocol.VerdictNotMet}))
	if got, want := s.Usage(), (protocol.Usage{InputTokens: 22, OutputTokens: 7}); got != want {
		t.Errorf("protocol.Usage = %+v, want %+v: every measured call and compaction counts, an evaluation does not", got, want)
	}
	if got, want := s.Context(), (protocol.ContextMeasured{Tokens: 100, Window: 1000, Source: "m"}); got != want {
		t.Errorf("Context = %+v, want %+v: a record with no prompt tokens keeps the reading", got, want)
	}
	if got := s.SubscriptionUsage(); !reflect.DeepEqual(got, second) {
		t.Errorf("protocol.SubscriptionUsage = %+v, want the newest snapshot %+v", got, second)
	}
	if got := s.CompactionCount(); got != 1 {
		t.Errorf("CompactionCount = %d, want 1", got)
	}
}

func TestBackendStateFoldsIntoAChain(t *testing.T) {
	head := json.RawMessage(`{"session_id":"S"}`)
	newer := json.RawMessage(`{"session_id":"S","turn":"t1"}`)
	for _, tc := range []struct {
		name   string
		events []protocol.Payload
		want   BackendChain
		err    string
	}{
		{"a chunk starts a chain", []protocol.Payload{protocol.BackendState{Backend: "cc", Head: head, Chunk: "cc-3-0", Restart: true, Entries: 2, Sum: "s2"}},
			BackendChain{Head: head, Chunks: []string{"cc-3-0"}, Entries: 2, Sum: "s2"}, ""},
		{"a chunk continues the chain", []protocol.Payload{
			protocol.BackendState{Backend: "cc", Head: head, Chunk: "cc-3-0", Restart: true, Entries: 2, Sum: "s2"},
			protocol.BackendState{Backend: "cc", Head: head, Chunk: "cc-3-1", Entries: 3, Sum: "s3"}},
			BackendChain{Head: head, Chunks: []string{"cc-3-0", "cc-3-1"}, Entries: 3, Sum: "s3"}, ""},
		{"a record with no chunk replaces the head", []protocol.Payload{
			protocol.BackendState{Backend: "cc", Head: head, Chunk: "cc-3-0", Restart: true, Entries: 2, Sum: "s2"},
			protocol.BackendState{Backend: "cc", Head: newer, Entries: 2, Sum: "s2"}},
			BackendChain{Head: newer, Chunks: []string{"cc-3-0"}, Entries: 2, Sum: "s2"}, ""},
		{"a restart drops the chunks of the chain", []protocol.Payload{
			protocol.BackendState{Backend: "cc", Head: head, Chunk: "cc-3-0", Restart: true, Entries: 2, Sum: "s2"},
			protocol.BackendState{Backend: "cc", Head: newer, Chunk: "cc-5-0", Restart: true, Entries: 1, Sum: "t1"}},
			BackendChain{Head: newer, Chunks: []string{"cc-5-0"}, Entries: 1, Sum: "t1"}, ""},
		{"a restart with no chunk empties the chain", []protocol.Payload{
			protocol.BackendState{Backend: "cc", Head: head, Chunk: "cc-3-0", Restart: true, Entries: 2, Sum: "s2"},
			protocol.BackendState{Backend: "cc", Head: newer, Restart: true}},
			BackendChain{Head: newer}, ""},
		{"a one-blob record is a chain of its blob", []protocol.Payload{protocol.BackendState{Backend: "cc", BlobKey: "cc-3"}}, BackendChain{Legacy: "cc-3"}, ""},
		{"a restart replaces a one-blob record", []protocol.Payload{
			protocol.BackendState{Backend: "cc", BlobKey: "cc-3"},
			protocol.BackendState{Backend: "cc", Head: head, Chunk: "cc-5-0", Restart: true, Entries: 1, Sum: "s1"}},
			BackendChain{Head: head, Chunks: []string{"cc-5-0"}, Entries: 1, Sum: "s1"}, ""},
		{"a chunk cannot continue a one-blob record", []protocol.Payload{
			protocol.BackendState{Backend: "cc", BlobKey: "cc-3"},
			protocol.BackendState{Backend: "cc", Head: head, Chunk: "cc-5-0", Entries: 1, Sum: "s1"}}, BackendChain{}, "continues a one-blob state"},
		{"a one-blob record has no chain fields", []protocol.Payload{protocol.BackendState{Backend: "cc", BlobKey: "cc-3", Head: head}}, BackendChain{}, "one blob"},
		{"a chain record has a head", []protocol.Payload{protocol.BackendState{Backend: "cc", Chunk: "cc-3-0", Restart: true, Entries: 1}}, BackendChain{}, "needs a head"},
		{"a chunk adds entries", []protocol.Payload{
			protocol.BackendState{Backend: "cc", Head: head, Chunk: "cc-3-0", Restart: true, Entries: 2, Sum: "s2"},
			protocol.BackendState{Backend: "cc", Head: head, Chunk: "cc-3-1", Entries: 2, Sum: "s2"}}, BackendChain{}, "entries"},
		{"a record with no chunk keeps the entries", []protocol.Payload{
			protocol.BackendState{Backend: "cc", Head: head, Chunk: "cc-3-0", Restart: true, Entries: 2, Sum: "s2"},
			protocol.BackendState{Backend: "cc", Head: head, Entries: 3, Sum: "s3"}}, BackendChain{}, "entries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var s State
			for _, e := range with(base, tc.events...) {
				if err := s.Apply(record(t, s.Head()+1, e)); err != nil {
					if tc.err == "" || !strings.Contains(err.Error(), tc.err) {
						t.Fatalf("apply %s = %v, want %q", e.Kind(), err, tc.err)
					}
					return
				}
			}
			if tc.err != "" {
				t.Fatalf("no error, want %q", tc.err)
			}
			if got, _ := s.BackendState("cc"); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("chain = %+v, want %+v", got, tc.want)
			}
		})
	}
}
