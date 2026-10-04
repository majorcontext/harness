package eventlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func created() Event { return SessionCreated{Model: "openai/gpt-5", Origin: "cli"} }

func admit(id string) Event {
	return InputAdmitted{InputID: id, Delivery: DeliveryQueue, Source: "user", Parts: []Part{{Type: PartText, Text: "hi"}}}
}

func steer(id string) Event {
	return InputAdmitted{InputID: id, Delivery: DeliverySteer, Source: "user", Parts: []Part{{Type: PartText, Text: "hi"}}}
}

func start(turn string, inputs ...string) Event { return TurnStarted{TurnID: turn, InputIDs: inputs} }
func promote(in, turn string) Event             { return InputPromoted{InputID: in, TurnID: turn} }
func withdraw(in string) Event                  { return InputWithdrawn{InputID: in} }

func call(turn, item string, ids ...string) Event {
	m := Message{Role: RoleAssistant}
	for _, id := range ids {
		m.Parts = append(m.Parts, Part{Type: PartToolCall, CallID: id, Name: "bash", Arguments: json.RawMessage(`{"cmd":"ls"}`)})
	}
	return ItemCompleted{ItemID: item, TurnID: turn, Message: m}
}

func result(turn, item, id string) Event {
	return ItemCompleted{ItemID: item, TurnID: turn, Message: Message{Role: RoleTool, Parts: []Part{
		{Type: PartToolResult, CallID: id, Text: "ok"}}}}
}

func end(turn string, r StopReason, cause Cause) Event {
	return TurnEnded{TurnID: turn, StopReason: r, Error: string(cause)}
}

func ask(req, item string) Event {
	return RequestOpened{RequestID: req, ItemID: item, RequestKind: "question", Payload: json.RawMessage(`{"q":"which?"}`)}
}

func resolve(req string) Event {
	return RequestResolved{RequestID: req, Resolution: ResolutionAnswered, Answer: json.RawMessage(`"yes"`)}
}

func dismiss(req string) Event {
	return RequestResolved{RequestID: req, Resolution: ResolutionDismissed}
}
func cmd(id, status string) Event {
	return CommandRecorded{InputID: id, Line: "/x", Name: "x", Status: status}
}
func suspend(turn string, c Cause) Event { return TurnSuspended{TurnID: turn, Cause: c} }
func resume(turn string, n int) Event    { return TurnResumed{TurnID: turn, Count: n} }
func goal(s GoalState) Event             { return GoalChanged{State: s} }
func verdict(turn string) Event          { return GoalEvaluated{TurnID: turn, Verdict: VerdictNotMet} }

var setGoal Event = GoalSet{Condition: "tests pass", MaxTurns: 3}

func with(base []Event, more ...Event) []Event {
	return append(append([]Event(nil), base...), more...)
}

var (
	base      = []Event{created()}
	running   = with(base, admit("a"), start("t1", "a"))
	calling   = with(running, call("t1", "i1", "c1"))
	asking    = with(calling, ask("c1", "i1"))
	suspended = with(calling, result("t1", "i2", "c1"), suspend("t1", CauseHandoff))
	evaluated = with(base, setGoal, admit("a"), start("t1", "a"), end("t1", StopCompleted, ""), verdict("t1"))
)

type view struct {
	Status   Status
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

func record(t *testing.T, seq uint64, e Event) Record {
	t.Helper()
	data, err := Envelope{Seq: seq, Time: t0, Event: e}.Encode()
	if err != nil {
		t.Fatalf("encode %s: %v", e.Kind(), err)
	}
	return Record{Seq: seq, Data: data}
}

func replay(t *testing.T, events []Event) *State {
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
	events []Event
	err    string
	want   view
}{
	{"an admitted input waits in the queue", with(base, admit("a")), "", view{Status: StatusIdle, Queue: []string{"a"}}},
	{"a record before session.created fails", []Event{admit("a")}, "before session.created", view{}},
	{"session.created comes once", with(base, created()), "session already created", view{}},
	{"a turn takes its queued input", running, "", view{Status: StatusRunning, Turn: "t1"}},
	{"a steer input joins the running turn", with(running, steer("s"), promote("s", "t1")), "", view{Status: StatusRunning, Turn: "t1"}},
	{"a withdrawn input leaves the queue", with(base, admit("a"), withdraw("a")), "", view{Status: StatusIdle}},
	{"a promoted input is not withdrawn", with(running, withdraw("a")), "input a is promoted", view{}},
	{"an input drives one turn", with(running, end("t1", StopCompleted, ""), start("t2", "a")), "input a is promoted", view{}},
	{"a queue input waits for the next turn", with(running, admit("b"), promote("b", "t1")), "input b waits for the next turn", view{}},
	{"a withdrawn input is not promoted", with(base, admit("a"), withdraw("a"), admit("b"), start("t1", "b"), promote("a", "t1")), "input a is withdrawn", view{}},
	{"an input id is admitted once", with(base, admit("a"), admit("a")), "input a is admitted", view{}},
	{"a promotion needs its turn running", with(base, admit("a"), admit("b"), start("t1", "a"), promote("b", "t2")), "turn t2 is not running", view{}},
	{"a turn starts only with no current turn", with(running, admit("b"), start("t2", "b")), "turn t1 is current", view{}},
	{"a turn id is used once", with(running, end("t1", StopCompleted, ""), admit("b"), start("t1", "b")), "turn t1 was used", view{}},
	{"a turn starts only from queued inputs", with(base, start("t1", "x")), "input x is unknown", view{}},
	{"an item needs its turn running", with(running, call("t2", "i1", "c1")), "turn t2 is not running", view{}},
	{"a tool result closes its call", with(calling, result("t1", "i2", "c1"), end("t1", StopCompleted, "")), "", view{Status: StatusIdle}},
	{"a tool call gets one result", with(calling, result("t1", "i2", "c1"), result("t1", "i3", "c1")), "no open tool call c1", view{}},
	{"a tool call id is open once", with(calling, call("t1", "i2", "c1")), "tool call c1 is open", view{}},
	{"a turn does not end with an unanswered tool call", with(calling, end("t1", StopCompleted, "")), "tool call c1 has no result", view{}},
	{"a request holds a tool call open past the turn", with(asking, end("t1", StopAwaitingInput, "")), "", view{Status: StatusWaiting, Requests: []string{"c1"}, Calls: []string{"c1"}}},
	{"a request holds open only the call that it names", with(running, call("t1", "i1", "c1", "c2"), ask("c1", "i1"), end("t1", StopAwaitingInput, "")), "tool call c2 has no result", view{}},
	{"awaiting_input needs an open request", with(running, end("t1", StopAwaitingInput, "")), "no open request", view{}},
	{"awaiting_input needs a request from its own turn", with(asking, result("t1", "i2", "c1"), end("t1", StopCompleted, ""), start("t2"), end("t2", StopAwaitingInput, "")), "turn t2 awaits input with no open request from this turn", view{}},
	{"a turn does not start while a tool call is open", with(asking, end("t1", StopAwaitingInput, ""), start("t2")), "turn t2 starts with open tool call c1", view{}},
	{"an input is not admitted while a request is open", with(asking, admit("b")), "request c1 is open", view{}},
	{"a dismissed request lets an input start a turn", with(asking, end("t1", StopAwaitingInput, ""), dismiss("c1"), admit("b"), start("t2", "b")), "", view{Status: StatusRunning, Turn: "t2"}},
	{"an answer leaves the tool call open for the turn that it starts", with(asking, end("t1", StopAwaitingInput, ""), resolve("c1")), "", view{Status: StatusIdle, Calls: []string{"c1"}}},
	{"a turn starts while the call of an answer is open", with(asking, end("t1", StopAwaitingInput, ""), resolve("c1"), start("t2")), "", view{Status: StatusRunning, Turn: "t2", Calls: []string{"c1"}}},
	{"a result closes the call of an answer", with(asking, end("t1", StopAwaitingInput, ""), resolve("c1"), start("t2"), result("t2", "i2", "c1"), end("t2", StopCompleted, "")), "", view{Status: StatusIdle}},
	{"a turn does not end with the call of an answer unresulted", with(asking, end("t1", StopAwaitingInput, ""), resolve("c1"), start("t2"), end("t2", StopCompleted, "")), "tool call c1 has no result", view{}},
	{"a dismissal closes the tool call of its request", with(asking, end("t1", StopAwaitingInput, ""), dismiss("c1")), "", view{Status: StatusIdle}},
	{"a dismissal closes only the call that it names", with(running, call("t1", "i1", "c1", "c2"), ask("c1", "i1"), dismiss("c1")), "", view{Status: StatusRunning, Turn: "t1", Calls: []string{"c2"}}},
	{"a request opens on an open tool call named by its id", with(running, call("t1", "i1", "c1"), ask("c9", "i1")), "no open tool call c9", view{}},
	{"a request resolves once", with(asking, resolve("c1"), resolve("c1")), "request c1 is not open", view{}},
	{"a request opens only in a running turn", with(base, ask("c1", "i1")), "no running turn", view{}},
	{"a handoff suspends the turn", suspended, "", view{Status: StatusIdle, Turn: "t1 suspended"}},
	{"a suspended turn resumes", with(suspended, resume("t1", 1)), "", view{Status: StatusRunning, Turn: "t1"}},
	{"only a handoff suspends a turn", with(running, suspend("t1", CauseStopped)), "cause stopped", view{}},
	{"a turn suspends with no open tool call", with(calling, suspend("t1", CauseHandoff)), "turn t1 suspends with open tool call c1", view{}},
	{"an open request does not let a turn suspend", with(asking, suspend("t1", CauseHandoff)), "turn t1 suspends with open tool call c1", view{}},
	{"a resume counts each resume", with(suspended, resume("t1", 2)), "count 2, want 1", view{}},
	{"a stopped turn never resumes", with(running, end("t1", StopInterrupted, CauseStopped), resume("t1", 1)), "turn t1 is not suspended", view{}},
	{"a suspended turn does not end", with(suspended, end("t1", StopCompleted, "")), "turn t1 is suspended", view{}},
	{"an interrupted turn names its cause", with(running, end("t1", StopInterrupted, "")), "interrupt cause", view{}},
	{"a goal counts evaluated turns", evaluated, "", view{Status: StatusIdle, Goal: "active 1"}},
	{"a turn is evaluated once", with(evaluated, verdict("t1")), "turn t1 was evaluated", view{}},
	{"a new goal resets the count", with(evaluated, setGoal), "", view{Status: StatusIdle, Goal: "active 0"}},
	{"a paused goal becomes active", with(base, setGoal, goal(GoalPaused), goal(GoalActive)), "", view{Status: StatusIdle, Goal: "active 0"}},
	{"an achieved goal stays achieved", with(base, setGoal, goal(GoalAchieved), goal(GoalActive)), "goal achieved cannot become active", view{}},
	{"any goal clears", with(base, setGoal, goal(GoalExhausted), goal(GoalCleared)), "", view{Status: StatusIdle, Goal: "cleared 0"}},
	{"a cleared goal does not clear again", with(base, setGoal, goal(GoalCleared), goal(GoalCleared)), "goal cleared cannot become cleared", view{}},
	{"a new goal does not evaluate an ended turn", with(base, admit("a"), start("t1", "a"), end("t1", StopCompleted, ""), setGoal, verdict("t1")), "turn t1 was evaluated", view{}},
	{"a goal evaluates no more turns than max_turns", with(base, GoalSet{Condition: "x", MaxTurns: 1}, admit("a"), start("t1", "a"), end("t1", StopCompleted, ""), verdict("t1"), admit("b"), start("t2", "b"), end("t2", StopCompleted, ""), verdict("t2")), "max_turns 1 is reached", view{}},
	{"an evaluation names the last ended turn", with(running, setGoal, verdict("t1")), "turn t1 is not the last ended turn", view{}},
	{"an evaluation needs an active goal", with(running, end("t1", StopCompleted, ""), verdict("t1")), "no active goal", view{}},
	{"a compaction covers only earlier records", with(base, CompactionApplied{FromSeq: 1, ToSeq: 2}), "to_seq 2", view{}},
	{"a child settles after it spawns", with(base, ChildSettled{ChildID: "x", Outcome: OutcomeDone}), "child x is unknown", view{}},
	{"an adjusted goal keeps its turn count", with(evaluated, GoalSet{Condition: "y", MaxTurns: 3, Turns: 1}), "", view{Status: StatusIdle, Goal: "active 1"}},
	{"a goal starts below max_turns", with(base, GoalSet{Condition: "x", MaxTurns: 1, Turns: 1}), "turns", view{}},
	{"a settled child spawns again", with(base, ChildSpawned{ChildID: "x"}, ChildSettled{ChildID: "x", Outcome: OutcomeDone}, ChildSpawned{ChildID: "x"}), "", view{Status: StatusIdle}},
	{"an unsettled child does not spawn again", with(base, ChildSpawned{ChildID: "x"}, ChildSpawned{ChildID: "x"}), "already spawned", view{}},
	{"a settings change keeps a model", with(base, SettingsChanged{Model: new("")}), "empty model", view{}},
	{"an accepted command ends once", with(base, cmd("a", "accepted"), cmd("a", "succeeded")), "", view{Status: StatusIdle}},
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
	if err := Check(&State{}, []Event{nil}); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("Check = %v, want ErrUnknownKind", err)
	}
}

func TestCheckMatchesAppend(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event Event
		err   error
	}{
		{"a pointer event passes as its value", &InputAdmitted{InputID: "b", Delivery: DeliveryQueue}, nil},
		{"a malformed payload fails as it would at append", RequestOpened{RequestID: "r", ItemID: "i1", Payload: json.RawMessage(`{bad`)}, errAny},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Check(replay(t, calling), []Event{tc.event})
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
	q[0].Parts = append(q[0].Parts, Part{})
	s.Requests()[0].Payload[0] = 'x'
	in, _, _ := s.Input("b")
	in.Parts[0].Text = "x"
	s.History()[1].Parts[0].Arguments[0] = 'x'
	if !reflect.DeepEqual(s, replay(t, events)) {
		t.Fatal("a caller changed the state through an accessor")
	}
}

func TestRetainedIsACopy(t *testing.T) {
	s := replay(t, []Event{created(), ToolResultRetained{Handle: "trh_1", Tool: "bash", BlobKey: "k"}})
	s.Retained()[0].Handle = "x"
	if got := s.Retained()[0].Handle; got != "trh_1" {
		t.Errorf("handle after a caller write = %q, want trh_1", got)
	}
}

func TestRecordedUsageFoldsIntoTheState(t *testing.T) {
	first := &SubscriptionUsage{Provider: "codex", Plan: "plus", CapturedAt: 1, Windows: []SubscriptionUsageWindow{{Key: "primary", UsedPercent: 12}}}
	second := &SubscriptionUsage{Provider: "codex", Plan: "plus", CapturedAt: 2, Windows: []SubscriptionUsageWindow{{Key: "primary", UsedPercent: 15}}}
	s := replay(t, with(base, setGoal, admit("a"), start("t1", "a"),
		ContextMeasured{Tokens: 100, Window: 1000, Source: "m", Usage: Usage{InputTokens: 10, OutputTokens: 2}, SubscriptionUsage: first},
		ContextMeasured{Usage: Usage{InputTokens: 5, OutputTokens: 1}, SubscriptionUsage: second},
		ContextMeasured{Usage: Usage{OutputTokens: 1}},
		end("t1", StopFailed, "boom"),
		CompactionApplied{FromSeq: 1, ToSeq: 3, Summary: "s", Usage: Usage{InputTokens: 7, OutputTokens: 3}},
		GoalEvaluated{TurnID: "t1", Verdict: VerdictNotMet}))
	if got, want := s.Usage(), (Usage{InputTokens: 22, OutputTokens: 7}); got != want {
		t.Errorf("Usage = %+v, want %+v: every measured call and compaction counts, an evaluation does not", got, want)
	}
	if got, want := s.Context(), (ContextMeasured{Tokens: 100, Window: 1000, Source: "m"}); got != want {
		t.Errorf("Context = %+v, want %+v: a record with no prompt tokens keeps the reading", got, want)
	}
	if got := s.SubscriptionUsage(); !reflect.DeepEqual(got, second) {
		t.Errorf("SubscriptionUsage = %+v, want the newest snapshot %+v", got, second)
	}
	if got := s.CompactionCount(); got != 1 {
		t.Errorf("CompactionCount = %d, want 1", got)
	}
}
