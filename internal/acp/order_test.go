package acp

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
)

type orderLog struct {
	mu     sync.Mutex
	events []string
}

func (l *orderLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *orderLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

// queuedResult is a result whose AfterSent releases queued updates. It waits
// until every other goroutine is blocked, so a request that is released too
// early has run by then.
type queuedResult struct{ log *orderLog }

func (r queuedResult) AfterSent() {
	synctest.Wait()
	r.log.add("queued updates sent")
}

func TestNextRequestOfASessionRunsAfterTheQueuedUpdatesAreSent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := &agent{}
		var log orderLog
		first, second := a.reserve("s"), a.reserve("s")

		answer, err := a.ticketed(context.WithValue(t.Context(), ticketKey{}, first), "s", func() (any, error) {
			return queuedResult{&log}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			_, _ = a.ticketed(context.WithValue(t.Context(), ticketKey{}, second), "s", func() (any, error) {
				log.add("next request runs")
				return nil, nil
			})
		}()
		synctest.Wait()
		if got := log.get(); len(got) != 0 {
			t.Fatalf("events before the answer is sent = %v, want none", got)
		}
		answer.(interface{ AfterSent() }).AfterSent()
		synctest.Wait()
		want := []string{"queued updates sent", "next request runs"}
		if got := log.get(); !slices.Equal(got, want) {
			t.Errorf("events = %v, want %v: a close or a second load must not run before the queued updates are sent", got, want)
		}
	})
}

func TestUpdatesWaitForTheAnswerOfALoadThatStartsAPump(t *testing.T) {
	a := &agent{}
	var sent []string
	held, release := holdSends(func(method string, params any) error {
		sent = append(sent, params.(string))
		return nil
	})
	answer := releasing{result: setupResult{after: release}, t: a.reserve("s")}

	_ = held("session/update", "live update")
	if len(sent) != 0 {
		t.Fatalf("sent %v before the answer, want none", sent)
	}
	answer.AfterSent()
	if !slices.Equal(sent, []string{"live update"}) {
		t.Errorf("sent %v after the answer, want the live update", sent)
	}
}

func TestQueueIsNotReleasedWhenAnotherSessionWasAdopted(t *testing.T) {
	started, other := &held{}, &held{}
	called := false
	release := func() { called = true }

	if got := queueRelease(started, other, release); got != nil {
		t.Error("queueRelease returned a release for a session that adopt replaced")
	}
	if got := queueRelease(started, started, release); got == nil {
		t.Fatal("queueRelease dropped the release of the adopted session")
	} else {
		got()
	}
	if !called {
		t.Error("the release of the adopted session did not run")
	}
}
