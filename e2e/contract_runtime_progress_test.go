package e2e

import (
	"errors"
	"reflect"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/protocol"
)

func TestContractRuntimeProgress(t *testing.T) {
	skipShort(t)
	rows := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"advance_folds_the_events_of_a_session_to_the_state_of_its_snapshot", rowAdvanceFoldsTheEventsOfASessionToTheStateOfItsSnapshot},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			row.run(t)
		})
	}
}

func rowAdvanceFoldsTheEventsOfASessionToTheStateOfItsSnapshot(t *testing.T) {
	r, s, _ := readerRuntime(t)
	if err := s.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored, err := r.Read(t.Context(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	var p, mid protocol.Progress
	for e, err := range stored.Events(t.Context(), 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Ephemeral {
			continue
		}
		if p, err = harness.Advance(p, e); err != nil {
			t.Fatalf("Advance %s at seq %d: %v", e.Kind, e.Seq, err)
		}
		if mid.Seq == 0 && p.Status == protocol.StatusRunning {
			mid = p
		}
	}
	snap := stored.Snapshot()
	if p.Seq != snap.HeadSeq || p.Status != snap.Status || p.TurnID != snap.TurnID || !reflect.DeepEqual(p.LastTurn, snap.LastTurn) ||
		!reflect.DeepEqual(p.Goal, snap.Goal) || len(p.Queued) != len(snap.Queued) {
		t.Errorf("folded Progress = %+v, want the snapshot %+v", p, snap)
	}
	if p.Status != protocol.StatusIdle || p.LastTurn == nil || p.LastTurn.StopReason != protocol.StopCompleted {
		t.Errorf("folded Progress = %+v, want an idle session whose turn completed", p)
	}
	rest := mid
	for e, err := range stored.Events(t.Context(), mid.Seq) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Ephemeral {
			continue
		}
		if rest, err = harness.Advance(rest, e); err != nil {
			t.Fatalf("Advance %s at seq %d from a stored cursor: %v", e.Kind, e.Seq, err)
		}
	}
	if _, err := harness.Advance(mid, protocol.Event{Seq: mid.Seq + 2, Kind: protocol.KindOwnerAcquired, Data: []byte(`{}`)}); !errors.Is(err, protocol.ErrSeq) {
		t.Errorf("Advance of an event that skips a seq = %v, want protocol.ErrSeq", err)
	}
	if !reflect.DeepEqual(rest, p) {
		t.Errorf("Progress resumed from seq %d = %+v, want %+v", mid.Seq, rest, p)
	}
}
