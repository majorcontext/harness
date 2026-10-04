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
)

func TestEnvelopeWireShape(t *testing.T) {
	got, err := Envelope{Seq: 42, Time: t0, Event: TurnEnded{TurnID: "t1", StopReason: StopCompleted, Usage: Usage{InputTokens: 1, OutputTokens: 2}}}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"v":1,"seq":42,"t":"2026-10-02T12:00:00Z","k":"turn.ended","d":{"turn_id":"t1","stop_reason":"completed","usage":{"input_tokens":1,"output_tokens":2}}}`
	if string(got) != want {
		t.Fatalf("Encode =\n%s\nwant\n%s", got, want)
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

func everyKind() []Event {
	return []Event{
		SessionCreated{ParentID: "p", Model: "openai/gpt-5", Settings: Settings{Effort: "high"}, Origin: "cli"},
		OwnerAcquired{Epoch: 7, Owner: "box-1"},
		SettingsChanged{ServiceTier: new("flex")},
		GoalSet{Condition: "tests pass", MaxTurns: 3},
		admit("a"), start("t1", "a"), steer("s"), promote("s", "t1"),
		ChildSpawned{ChildID: "k", Agent: "explore"},
		call("t1", "i1", "c1"), ask("r1", "i1"),
		RequestResolved{RequestID: "r1", Resolution: ResolutionAnswered, Answer: json.RawMessage(`"yes"`)},
		ItemCompleted{ItemID: "i2", TurnID: "t1", Message: Message{Role: RoleAssistant, Parts: []Part{{Type: PartText, Text: "done"}}}},
		ContextMeasured{Tokens: 900, Window: 1000, Source: "provider"},
		BackendState{Backend: "codex", BlobKey: "b1"},
		ToolResultRetained{Handle: "trh_1", Tool: "bash", BlobKey: "trh_1-2", Bytes: 20000, Lines: 3, Head: "x"},
		suspend("t1", CauseHandoff), resume("t1", 1),
		CompactionApplied{FromSeq: 1, ToSeq: 12, Summary: "so far", ByBackend: true},
		end("t1", StopCompleted, ""),
		GoalEvaluated{TurnID: "t1", Verdict: VerdictMet},
		goal(GoalAchieved),
		ChildSettled{ChildID: "k", Outcome: OutcomeDone, ResultRef: "blob/k"},
		admit("b"), withdraw("b"),
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
	if got, want := slices.Sorted(maps.Keys(kinds)), slices.Sorted(maps.Keys(registry)); !slices.Equal(got, want) {
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
	want := Summary{ParentID: "p", Origin: "cli", Model: "openai/gpt-5", Status: StatusIdle, Goal: GoalAchieved,
		HeadSeq: 25, CreatedAt: t0, UpdatedAt: t0.Add(24 * time.Second)}
	if got := live.Summary(); got != want {
		t.Fatalf("Summary = %+v, want %+v", got, want)
	}
	if got := live.Settings(); got != (Settings{Effort: "high", ServiceTier: "flex"}) {
		t.Fatalf("Settings = %+v", got)
	}
	if live.Usage().InputTokens != 10 || live.Context().Tokens != 900 || live.BackendState("codex") != "b1" || len(live.Retained()) != 1 {
		t.Fatalf("Usage = %+v, Context = %+v, BackendState = %q, Retained = %+v", live.Usage(), live.Context(), live.BackendState("codex"), live.Retained())
	}
}
