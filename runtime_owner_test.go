package harness_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

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
		name     string
		ownsLoop bool
		last     eventlog.Message
		late     []eventlog.Message
		want     []string
	}{
		{"the turn suspends after its last item", true, say("partial"), nil,
			[]string{"item.completed assistant partial"}},
		{"a running tool finishes within the budget", true, callTool("c1"), []eventlog.Message{toolResult("c1")},
			[]string{"item.completed assistant c1", "item.completed tool c1 ok"}},
		{"a tool call after the handoff is refused", false, say("partial"), []eventlog.Message{callTool("c2")},
			[]string{"item.completed assistant partial"}},
		{"a tool call that a loop-owning backend ran after the handoff is cut off", true, say("partial"),
			[]eventlog.Message{callTool("c2")},
			[]string{"item.completed assistant partial", "item.completed assistant c2", "item.completed tool c2 " + cutOff}},
		{"an open tool call is cut off", true, callTool("c1"), nil,
			[]string{"item.completed assistant c1", "item.completed tool c1 " + cutOff}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eachStore(t, func(t *testing.T, openStore func() harness.Store) {
				f1, f2 := newFake(), newFake()
				f1.ownsLoop, f1.late = tc.ownsLoop, tc.late
				r1 := runtime(t, openStore(), f1)
				submit(t, create(t, r1), text("a", "hi"))
				run := <-f1.runs
				run.emit(tc.last)
				closeRuntime(t, r1)
				r2 := runtime(t, openStore(), f2)
				open(t, r2)
				next := <-f2.runs
				if next.req.TurnID != run.req.TurnID || next.req.Input[0].Parts[0].Text != "hi" {
					t.Fatalf("resumed Request = %+v, want turn %s again", next.req, run.req.TurnID)
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

// crashMarker is the assistant item that closes a crashed turn.
const crashMarker = "[harness: this turn was interrupted by a process restart and could not complete]"

func TestOpenEndsACrashedTurn(t *testing.T) {
	crashed := []string{"item.completed assistant c1", "owner.acquired 1", "item.completed tool c1 " + cutOff, "item.completed assistant " + crashMarker, "turn.ended interrupted crashed"}
	for _, tc := range []struct {
		name    string
		queued  []protocol.Input
		want    []string
		started []string
		status  string
	}{
		{"with no queued input the session waits for input", nil, crashed, nil, protocol.StatusIdle},
		{"the same Open starts the next queued input", []protocol.Input{text("b", "two"), text("c", "three")},
			append(append([]string{"input.admitted b", "input.admitted c"}, crashed...), "turn.started b"),
			[]string{"two"}, protocol.StatusRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eachStore(t, func(t *testing.T, openStore func() harness.Store) {
				f1, f2 := newFake(), newFake()
				k := killable{make(chan struct{})}
				r1, err := harness.NewWithBackend(harness.Options{Store: openStore(), Owner: k}, f1)
				if err != nil {
					t.Fatal(err)
				}
				s1 := create(t, r1)
				submit(t, s1, text("a", "hi"))
				run := <-f1.runs
				for _, in := range tc.queued {
					submit(t, s1, in)
				}
				run.emit(callTool("c1"))
				close(k.lost)
				synctest.Wait()
				r2 := runtime(t, openStore(), f2)
				s := open(t, r2)
				var started []string
				select {
				case next := <-f2.runs:
					started = []string{next.req.Input[0].Parts[0].Text}
				default:
				}
				wantLog(t, openStore(), 4, tc.want...)
				if !slices.Equal(started, tc.started) || s.View().Status != tc.status {
					t.Fatalf("started %q with status %s, want %q with status %s", started, s.View().Status, tc.started, tc.status)
				}
				closeRuntime(t, r1)
				closeRuntime(t, r2)
			})
		})
	}
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
	_, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "fake/model"})
	if !errors.Is(err, boom) || errors.Is(err, harness.ErrSessionNotOwned) {
		t.Fatalf("Create = %v, want the store error only", err)
	}
}

func TestCloseWaitsForTheTurnsThatItsEndedContextCancels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.stuck = make(chan struct{})
		st := harness.NewMemStore()
		r := runtime(t, st, f)
		submit(t, create(t, r), text("a", "hi"))
		<-f.runs
		ctx, cancel := context.WithCancel(bg)
		cancel()
		closed := make(chan error, 1)
		go func() { closed <- r.Close(ctx) }()
		synctest.Wait()
		select {
		case err := <-closed:
			t.Fatalf("Close returned %v while a canceled turn still runs", err)
		default:
		}
		close(f.stuck)
		if err := <-closed; !errors.Is(err, context.Canceled) {
			t.Fatalf("Close = %v, want Canceled", err)
		}
		wantLog(t, st, 2, "input.admitted a", "turn.started a")
	})
}

func TestCloseWithAnEndedContextNeverHandsOff(t *testing.T) {
	for range 300 {
		synctest.Test(t, func(t *testing.T) {
			f := newFake()
			st := harness.NewMemStore()
			r := runtime(t, st, f)
			submit(t, create(t, r), text("a", "hi"))
			<-f.runs
			ctx, cancel := context.WithCancel(bg)
			cancel()
			if err := r.Close(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("Close = %v, want Canceled", err)
			}
			wantLog(t, st, 2, "input.admitted a", "turn.started a")
		})
	}
}

func TestFenceStopsTheStaleOwner(t *testing.T) {
	eachStore(t, func(t *testing.T, openStore func() harness.Store) {
		st := openStore()
		r1, r2 := runtime(t, st, newFake()), runtime(t, st, newFake())
		stale := create(t, r1)
		if again := open(t, r1); again != stale {
			t.Fatal("Open of a running session returned another Session")
		}
		if _, err := r1.Create(bg, protocol.CreateSession{ID: "s1", Model: "fake/model"}); !errors.Is(err, harness.ErrSessionExists) {
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
			[]string{"input.promoted s", "item.completed assistant steered: OPERATOR MESSAGES (address these, then continue the task):\n1. now", "turn.ended completed"}},
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

func TestViewsReturnACopyOfQueued(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, f := harness.NewMemStore(), newFake()
		r := runtime(t, st, f)
		s := create(t, r)
		submit(t, s, text("a", "hi"))
		run := <-f.runs
		submit(t, s, text("b", "next"))
		v, err := harness.OpenView(bg, st, "s1")
		if err != nil {
			t.Fatal(err)
		}
		s.View().Queued[0] = "x"
		v.Session().Queued[0] = "x"
		if got := s.View().Queued[0]; got != "b" {
			t.Fatalf("Session.View Queued[0] = %q after a caller write, want b", got)
		}
		if got := v.Session().Queued[0]; got != "b" {
			t.Fatalf("View.Session Queued[0] = %q after a caller write, want b", got)
		}
		run.end()
		(<-f.runs).end()
		closeRuntime(t, r)
	})
}

func TestOpenViewEventsEndAtTheHeadItRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, f := harness.NewMemStore(), newFake()
		r := runtime(t, st, f)
		s := create(t, r)
		v, err := harness.OpenView(bg, st, "s1")
		if err != nil {
			t.Fatal(err)
		}
		submit(t, s, text("a", "hi"))
		var seqs []uint64
		for e, err := range v.Events(bg, 0) {
			if err != nil {
				t.Fatal(err)
			}
			seqs = append(seqs, e.Seq)
		}
		if want := v.Session().HeadSeq; len(seqs) == 0 || seqs[len(seqs)-1] != want {
			t.Fatalf("View.Events seqs = %v, want the last to be %d", seqs, want)
		}
		(<-f.runs).end()
		closeRuntime(t, r)
	})
}

// blocking is an Owner whose Acquire waits for ctx and reports that it started.
type blocking struct{ entered chan struct{} }

func (b blocking) Acquire(ctx context.Context, _ string) (harness.Ownership, error) {
	close(b.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestCloseEndsAnInputThatWaitsForOwnership(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := blocking{entered: make(chan struct{})}
		r, err := harness.NewWithBackend(harness.Options{Store: harness.NewMemStore(), Owner: b}, newFake())
		if err != nil {
			t.Fatal(err)
		}
		rec, done := httptest.NewRecorder(), make(chan struct{})
		go func() {
			r.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/sessions/s1/inputs", strings.NewReader(`{"id":"a","parts":[{"type":"text","text":"hi"}]}`)))
			close(done)
		}()
		<-b.entered
		ctx, cancel := context.WithCancel(bg)
		cancel()
		if err := r.Close(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Close = %v, want context.Canceled", err)
		}
		<-done
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), protocol.CodeDraining) {
			t.Fatalf("POST inputs = %d %s, want 503 %s", rec.Code, rec.Body, protocol.CodeDraining)
		}
	})
}

// deniedOwner is an Owner that refuses every grant with err.
type deniedOwner struct{ err error }

func (o deniedOwner) Acquire(context.Context, string) (harness.Ownership, error) { return nil, o.err }

func TestAnOwnerRefusalIsSessionNotOwned(t *testing.T) {
	boom := errors.New("lease held elsewhere")
	r, err := harness.NewWithBackend(harness.Options{Store: harness.NewMemStore(), Owner: deniedOwner{boom}}, newFake())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Open(bg, "s1"); !errors.Is(err, harness.ErrSessionNotOwned) || !errors.Is(err, boom) {
		t.Fatalf("Open = %v, want ErrSessionNotOwned that wraps the refusal", err)
	}
	body := strings.NewReader(`{"id":"a","parts":[{"type":"text","text":"hi"}]}`)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/sessions/s1/inputs", body))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), protocol.CodeSessionNotOwned) {
		t.Fatalf("POST inputs = %d %s, want 409 %s", rec.Code, rec.Body, protocol.CodeSessionNotOwned)
	}
}
