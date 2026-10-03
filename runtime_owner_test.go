package harness_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/protocol"
)

// killable is an Owner whose one grant ends when the test closes lost.
type killable struct{ lost chan struct{} }

func (k killable) Acquire(context.Context, string) (harness.Ownership, error) { return k, nil }
func (k killable) Epoch() uint64                                              { return 1 }
func (k killable) Lost() <-chan struct{}                                      { return k.lost }
func (k killable) Release()                                                   {}

// held is a Store whose Append fails with err, or waits for hold while armed.
type held struct {
	harness.Store
	err   error
	armed atomic.Bool
	hold  chan struct{}
}

func (h *held) Append(ctx context.Context, id string, expectedSeq uint64, records ...[]byte) error {
	if h.armed.Load() {
		<-h.hold
	}
	if h.err != nil {
		return h.err
	}
	return h.Store.Append(ctx, id, expectedSeq, records...)
}

func open(t *testing.T, r *harness.Runtime) *harness.Session {
	t.Helper()
	s, err := r.Open(bg, "s1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	synctest.Wait()
	return s
}

func TestHandoffSuspendsAtAnItemBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		last eventlog.Message
		late []eventlog.Message
		want []string
	}{
		{"the turn suspends after its last item", say("partial"), nil,
			[]string{"item.completed assistant partial"}},
		{"a running tool finishes within the budget", callTool("c1"), []eventlog.Message{toolResult("c1")},
			[]string{"item.completed assistant c1", "item.completed tool c1 ok"}},
		{"a tool call after the handoff is refused", say("partial"), []eventlog.Message{callTool("c2")},
			[]string{"item.completed assistant partial"}},
		{"an open tool call is cut off", callTool("c1"), nil,
			[]string{"item.completed assistant c1", "item.completed tool c1 " + cutOff}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eachStore(t, func(t *testing.T, openStore func() harness.Store) {
				f1, f2 := newFake(), newFake()
				f1.late = tc.late
				r1 := runtime(t, openStore(), f1)
				submit(t, create(t, r1), text("a", "hi"))
				run := <-f1.runs
				run.emit(tc.last)
				closeRuntime(t, r1)
				r2 := runtime(t, openStore(), f2)
				open(t, r2)
				next := <-f2.runs
				if next.req.TurnID != run.req.TurnID || next.req.Resumed != 1 || next.req.Input[0].Parts[0].Text != "hi" {
					t.Fatalf("resumed Request = %+v, want turn %s resumed once", next.req, run.req.TurnID)
				}
				next.emit(say("rest"))
				next.end()
				want := append(append([]string{"input.admitted a", "turn.started a"}, tc.want...), "turn.suspended handoff",
					"owner.acquired 1", "turn.resumed 1", "item.completed assistant rest", "turn.ended completed")
				wantLog(t, openStore(), 2, want...)
				closeRuntime(t, r2)
			})
		})
	}
}

func TestOpenEndsACrashedTurn(t *testing.T) {
	eachStore(t, func(t *testing.T, openStore func() harness.Store) {
		f1, f2 := newFake(), newFake()
		k := killable{make(chan struct{})}
		r1, err := harness.NewWithBackend(harness.Options{Store: openStore(), Owner: k}, f1)
		if err != nil {
			t.Fatal(err)
		}
		submit(t, create(t, r1), text("a", "hi"))
		(<-f1.runs).emit(callTool("c1"))
		close(k.lost)
		synctest.Wait()
		r2 := runtime(t, openStore(), f2)
		s := open(t, r2)
		noRun(t, f2)
		wantLog(t, openStore(), 4, "item.completed assistant c1", "owner.acquired 1",
			"item.completed tool c1 "+cutOff, "turn.ended interrupted crashed")
		if v := s.View(); v.Status != protocol.StatusIdle || v.TurnID != "" {
			t.Fatalf("View = %+v, want idle", v)
		}
		closeRuntime(t, r1)
		closeRuntime(t, r2)
	})
}

func TestLostStopsTheActorBeforeItsNextAppend(t *testing.T) {
	eachStore(t, func(t *testing.T, openStore func() harness.Store) {
		st, f, k := &held{Store: openStore(), hold: make(chan struct{})}, newFake(), killable{make(chan struct{})}
		r, err := harness.NewWithBackend(harness.Options{Store: st, Owner: k}, f)
		if err != nil {
			t.Fatal(err)
		}
		s := create(t, r)
		submit(t, s, text("a", "one"))
		run := <-f.runs
		submit(t, s, text("b", "two"))
		st.armed.Store(true)
		run.end()
		close(k.lost)
		close(st.hold)
		noRun(t, f)
		wantLog(t, st, 2, "input.admitted a", "turn.started a", "input.admitted b", "turn.ended completed")
		closeRuntime(t, r)
	})
}

func TestCreateReturnsTheStoreError(t *testing.T) {
	boom := errors.New("disk full")
	r := runtime(t, &held{Store: harness.NewMemStore(), err: boom}, newFake())
	_, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "test/model"})
	if !errors.Is(err, boom) || errors.Is(err, harness.ErrSessionNotOwned) {
		t.Fatalf("Create = %v, want the store error only", err)
	}
}

func TestCloseReturnsWhenItsContextEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.stuck = make(chan struct{})
		r := runtime(t, harness.NewMemStore(), f)
		submit(t, create(t, r), text("a", "hi"))
		<-f.runs
		ctx, cancel := context.WithTimeout(bg, time.Minute)
		defer cancel()
		if err := r.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close = %v, want DeadlineExceeded", err)
		}
		close(f.stuck)
		synctest.Wait()
	})
}

func TestFenceStopsTheStaleOwner(t *testing.T) {
	eachStore(t, func(t *testing.T, openStore func() harness.Store) {
		st := openStore()
		r1, r2 := runtime(t, st, newFake()), runtime(t, st, newFake())
		stale := create(t, r1)
		if again := open(t, r1); again != stale {
			t.Fatal("Open of a running session returned another Session")
		}
		if _, err := r1.Create(bg, protocol.CreateSession{ID: "s1", Model: "test/model"}); !errors.Is(err, harness.ErrSessionExists) {
			t.Fatalf("Create of an existing session = %v, want ErrSessionExists", err)
		}
		open(t, r2)
		for range 2 {
			if _, err := stale.Submit(bg, text("a", "hi")); !errors.Is(err, harness.ErrSessionNotOwned) {
				t.Fatalf("Submit on the stale owner = %v, want ErrSessionNotOwned", err)
			}
		}
		wantLog(t, st, 0, "session.created", "owner.acquired 1", "owner.acquired 1")
		closeRuntime(t, r1)
		closeRuntime(t, r2)
	})
}

func TestEventsResumeAfterARestart(t *testing.T) {
	eachStore(t, func(t *testing.T, openStore func() harness.Store) {
		f1, f2 := newFake(), newFake()
		r1 := runtime(t, openStore(), f1)
		submit(t, create(t, r1), text("a", "hi"))
		(<-f1.runs).end()
		closeRuntime(t, r1)
		r2 := runtime(t, openStore(), f2)
		s := open(t, r2)
		var got []string
		var seqs []uint64
		done := make(chan struct{})
		go func() {
			defer close(done)
			for e, err := range s.Events(bg, 3) {
				if err != nil {
					got = append(got, err.Error())
					return
				}
				seqs, got = append(seqs, e.Seq), append(got, line(e.Kind, e.Data))
				if e.Seq == 9 {
					return
				}
			}
		}()
		synctest.Wait()
		submit(t, s, text("b", "again"))
		(<-f2.runs).end()
		<-done
		want := []string{"turn.started a", "turn.ended completed", "owner.acquired 1",
			"input.admitted b", "turn.started b", "turn.ended completed"}
		if !slices.Equal(seqs, []uint64{4, 5, 6, 7, 8, 9}) || !slices.Equal(got, want) {
			t.Fatalf("Events(3) = %v %q, want seqs 4..9 %q", seqs, got, want)
		}
		closeRuntime(t, r2)
	})
}

func TestOpenViewReadsWithoutAppending(t *testing.T) {
	eachStore(t, func(t *testing.T, openStore func() harness.Store) {
		st, f := openStore(), newFake()
		r := runtime(t, st, f)
		s := create(t, r)
		submit(t, s, text("a", "hi"))
		run := <-f.runs
		v, err := harness.OpenView(bg, st, "s1")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(v.Session(), s.View()) || v.Session().Status != protocol.StatusRunning {
			t.Fatalf("OpenView = %+v, want %+v", v.Session(), s.View())
		}
		var kinds []string
		for e, err := range v.Events(bg, 2) {
			if err != nil {
				t.Fatal(err)
			}
			kinds = append(kinds, e.Kind)
		}
		if want := []string{"input.admitted", "turn.started"}; !slices.Equal(kinds, want) {
			t.Fatalf("View.Events(2) = %v, want %v", kinds, want)
		}
		if page, err := r.List(bg, protocol.ListSessions{}); err != nil || len(page.Sessions) != 1 || page.Sessions[0].Status != protocol.StatusRunning {
			t.Fatalf("List = %+v, %v", page, err)
		}
		if _, err := harness.OpenView(bg, st, "s2"); !errors.Is(err, harness.ErrSessionNotFound) {
			t.Fatalf("OpenView of a missing session = %v, want ErrSessionNotFound", err)
		}
		run.end()
		wantLog(t, st, 3, "turn.started a", "turn.ended completed")
		closeRuntime(t, r)
	})
}

func TestSteerInput(t *testing.T) {
	for _, tc := range []struct {
		name           string
		steering, deaf bool
		want           []string
	}{
		{"a steering backend folds the input in at the next item", true, false,
			[]string{"input.promoted s", "item.completed assistant steered: now", "turn.ended completed"}},
		{"another backend runs the input as the next turn", false, false,
			[]string{"turn.ended completed", "turn.started s", "turn.ended completed"}},
		{"a steering backend that ends without taking the input runs it as the next turn", true, true,
			[]string{"turn.ended completed", "turn.started s", "turn.ended completed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eachStore(t, func(t *testing.T, openStore func() harness.Store) {
				st, f := openStore(), newFake()
				f.steering, f.deaf = tc.steering, tc.deaf
				r := runtime(t, st, f)
				s := create(t, r)
				submit(t, s, text("a", "hi"))
				run := <-f.runs
				steer := text("s", "now")
				steer.Delivery, steer.ExpectedTurnID = protocol.DeliverySteer, "turn_other"
				if _, err := s.Submit(bg, steer); !errors.Is(err, harness.ErrTurnMismatch) {
					t.Fatalf("Submit for another turn = %v, want ErrTurnMismatch", err)
				}
				steer.ExpectedTurnID = run.req.TurnID
				submit(t, s, steer)
				run.emit(say("next"))
				run.end()
				if !tc.steering || tc.deaf {
					(<-f.runs).end()
				}
				wantLog(t, st, 4, append([]string{"input.admitted s", "item.completed assistant next"}, tc.want...)...)
				closeRuntime(t, r)
			})
		})
	}
}
