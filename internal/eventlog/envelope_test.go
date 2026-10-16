package eventlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/majorcontext/harness/protocol"
)

func TestEnvelopeWireShape(t *testing.T) {
	for _, tc := range []struct {
		event protocol.Payload
		want  string
	}{
		{protocol.TurnEnded{TurnID: "t1", StopReason: protocol.StopCompleted},
			`{"v":1,"seq":42,"t":"2026-10-02T12:00:00Z","k":"turn.ended","d":{"turn_id":"t1","stop_reason":"completed"}}`},
		{protocol.ContextMeasured{Tokens: 7, Window: 100, Source: "m", Usage: protocol.Usage{InputTokens: 1, OutputTokens: 2}},
			`{"v":1,"seq":42,"t":"2026-10-02T12:00:00Z","k":"context.measured","d":{"tokens":7,"window":100,"source":"m","usage":{"input_tokens":1,"output_tokens":2}}}`},
	} {
		got, err := Envelope{Seq: 42, Time: t0, Event: tc.event}.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.want {
			t.Fatalf("Encode =\n%s\nwant\n%s", got, tc.want)
		}
	}
}

type unregistered struct{}

func (unregistered) Kind() string { return "made.up" }

func TestCodecRejects(t *testing.T) {
	line := string(record(t, 1, created()).Data)
	if _, err := (Envelope{Seq: 1, Event: unregistered{}}).Encode(); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("Encode of an unregistered kind = %v, want ErrUnknownKind", err)
	}
	rows := []struct {
		name string
		rec  Record
		want error
	}{
		{"unknown kind", Record{Seq: 1, Data: []byte(`{"v":1,"seq":1,"t":"2026-10-02T12:00:00Z","k":"made.up","d":{}}`)}, ErrUnknownKind},
		{"unknown version", Record{Seq: 1, Data: []byte(`{"v":2,"seq":1,"t":"2026-10-02T12:00:00Z","k":"session.created","d":{}}`)}, ErrVersion},
		{"torn line", Record{Seq: 1, Data: []byte(line[:len(line)/2])}, nil},
		{"record and envelope seq differ", Record{Seq: 2, Data: []byte(line)}, ErrSeq},
		{"seq gap", record(t, 2, created()), ErrSeq},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			var s State
			err := s.Apply(tc.rec)
			if err == nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("Apply = %v, want %v", err, tc.want)
			}
			if !reflect.DeepEqual(s, State{}) {
				t.Fatal("a failed Apply changed the state")
			}
		})
	}
}

func everyKind() []protocol.Payload {
	return []protocol.Payload{
		protocol.SessionCreated{Model: "openai/gpt-5", Settings: protocol.Settings{Effort: "high"}, Origin: "cli"},
		protocol.OwnerAcquired{Epoch: 7, Owner: "box-1"},
		protocol.SettingsChanged{ServiceTier: new("flex")},
		protocol.GoalSet{Condition: "tests pass", MaxTurns: 3},
		admit("a"), start("t1", "a"), steer("s"), promote("s", "t1"),
		protocol.ChildSpawned{ChildID: "k", Agent: "explore"},
		call("t1", "i1", "c1"), ask("c1", "i1"),
		protocol.RequestResolved{RequestID: "c1", Resolution: protocol.ResolutionAnswered, Answer: json.RawMessage(`"yes"`)}, result("t1", "i3", "c1"),
		protocol.ItemCompleted{ItemID: "i2", TurnID: "t1", Message: protocol.LogMessage{Role: protocol.RoleAssistant, Parts: []protocol.LogPart{{Type: protocol.LogPartText, Text: "done"}}}},
		protocol.ContextMeasured{Tokens: 900, Window: 1000, Source: "provider", Usage: protocol.Usage{InputTokens: 10, OutputTokens: 2},
			SubscriptionUsage: &protocol.SubscriptionUsage{Provider: "codex", Plan: "plus", CapturedAt: 5,
				Windows: []protocol.SubscriptionUsageWindow{{Key: "primary", Label: "5-hour", UsedPercent: 12.5, ResetsAt: 9}},
				Overage: &protocol.SubscriptionOverage{InUse: true, Status: "allowed", ResetsAt: 11}}},
		protocol.BackendState{Backend: "codex", BlobKey: "b1"},
		protocol.ToolResultRetained{Handle: "trh_1", Tool: "bash", BlobKey: "trh_1-2", Bytes: 20000, Lines: 3, Head: "x"},
		suspend("t1", protocol.CauseHandoff), resume("t1", 1),
		protocol.CompactionApplied{FromSeq: 1, ToSeq: 12, Summary: "so far", ByBackend: true},
		protocol.TurnEnded{TurnID: "t1", StopReason: protocol.StopCompleted, Error: "short", ErrorDetail: "short and longer"},
		protocol.GoalEvaluated{TurnID: "t1", Verdict: protocol.VerdictMet},
		goal(protocol.GoalAchieved),
		protocol.ChildSettled{ChildID: "k", Outcome: protocol.OutcomeDone, ResultRef: "blob/k"},
		admit("b"), withdraw("b"),
		admit("d"), start("t2", "d"), protocol.TurnEnded{TurnID: "t2", StopReason: protocol.StopFailed, Cause: protocol.CauseProviderExhausted, ResetsAt: 1},
		protocol.ContinueCancelled{ContinueAt: 1},
		protocol.CommandRecorded{InputID: "c", Line: "/thinking high", Name: "thinking", Args: map[string]any{"effort": "high"},
			Status: "succeeded", Text: "/thinking succeeded", Result: json.RawMessage(`{"effort":"high"}`)},
	}
}

func TestReplayMatchesLiveApply(t *testing.T) {
	events := everyKind()
	kinds := map[string]bool{}
	var live State
	var log bytes.Buffer
	for i, e := range events {
		kinds[e.Kind()] = true
		env := Envelope{Seq: uint64(i + 1), Time: t0.Add(time.Duration(i) * time.Second), Event: e}
		if err := live.apply(env); err != nil {
			t.Fatalf("live apply %s: %v", e.Kind(), err)
		}
		data, err := env.Encode()
		if err != nil {
			t.Fatal(err)
		}
		log.Write(append(data, '\n'))
	}
	if got, want := slices.Sorted(maps.Keys(kinds)), slices.Sorted(maps.Keys(goldenKinds())); !slices.Equal(got, want) {
		t.Fatalf("kinds = %v, want every registered kind %v", got, want)
	}
	var replayed State
	for i, line := range bytes.Split(bytes.TrimSuffix(log.Bytes(), []byte("\n")), []byte("\n")) {
		env, err := Decode(line)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(env.Event, events[i]) {
			t.Fatalf("decoded %+v, want %+v", env.Event, events[i])
		}
		if err := replayed.Apply(Record{Seq: uint64(i + 1), Data: line}); err != nil {
			t.Fatalf("replay %s: %v", env.Event.Kind(), err)
		}
	}
	if !reflect.DeepEqual(&replayed, &live) {
		t.Fatalf("replayed state differs from live state:\n%+v\n%+v", replayed, live)
	}
	want := Summary{Origin: "cli", Model: "openai/gpt-5", Status: protocol.StatusIdle, Goal: protocol.GoalAchieved,
		HeadSeq: 31, CreatedAt: t0, UpdatedAt: t0.Add(30 * time.Second)}
	if got := live.Summary(); got != want {
		t.Fatalf("Summary = %+v, want %+v", got, want)
	}
	if got := live.Settings(); got != (protocol.Settings{Effort: "high", ServiceTier: "flex"}) {
		t.Fatalf("Settings = %+v", got)
	}
	if live.Usage().InputTokens != 10 || live.Context().Tokens != 900 || backendBlob(live) != "b1" || len(live.Retained()) != 1 {
		t.Fatalf("Usage = %+v, Context = %+v, BackendState = %q, Retained = %+v", live.Usage(), live.Context(), backendBlob(live), live.Retained())
	}
}

func backendBlob(s State) string {
	c, _ := s.BackendState("codex")
	return c.Legacy
}

func TestAdvanceFoldsWhatStateFolds(t *testing.T) {
	waiting := with(asking, end("t1", protocol.StopAwaitingInput, ""))
	paused := with(evaluated, goal(protocol.GoalPaused), goal(protocol.GoalActive), goal(protocol.GoalPaused))
	for name, log := range map[string][]protocol.Payload{
		"every kind":         everyKind(),
		"a waiting session":  with(waiting, resolve("c1"), admit("b"), start("t2", "b"), result("t2", "i2", "c1")),
		"a dismissed answer": with(waiting, dismiss("c1"), admit("b")),
		"a suspended turn":   with(suspended, resume("t1", 1), end("t1", protocol.StopCompleted, "")),
		"a paused goal":      with(paused, goal(protocol.GoalCleared), setGoal),
	} {
		t.Run(name, func(t *testing.T) {
			var s State
			var p protocol.Progress
			for i, e := range log {
				rec := record(t, uint64(i+1), e)
				var ev protocol.Event
				if err := json.Unmarshal(rec.Data, &ev); err != nil {
					t.Fatal(err)
				}
				before, _ := json.Marshal(p)
				next, err := Advance(p, ev)
				if err != nil {
					t.Fatalf("Advance %s: %v", e.Kind(), err)
				}
				if after, _ := json.Marshal(p); string(before) != string(after) {
					t.Fatalf("Advance %s changed its input:\n%s\n%s", e.Kind(), before, after)
				}
				if err := s.Apply(rec); err != nil {
					t.Fatal(err)
				}
				if p = next; !reflect.DeepEqual(p, s.Progress()) {
					t.Fatalf("after %s, Advance = %+v, State = %+v", e.Kind(), p, s.Progress())
				}
			}
			ev := protocol.Event{Seq: uint64(len(log)) + 2, Kind: protocol.KindOwnerAcquired, Data: json.RawMessage(`{}`)}
			if _, err := Advance(p, ev); !errors.Is(err, ErrSeq) {
				t.Fatalf("Advance out of order = %v, want ErrSeq", err)
			}
			if _, err := Advance(p, protocol.Event{Seq: p.Seq + 1, Kind: protocol.KindStatus, Ephemeral: true}); !errors.Is(err, protocol.ErrUnknownKind) {
				t.Fatalf("Advance of an ephemeral frame = %v, want ErrUnknownKind", err)
			}
		})
	}
}
