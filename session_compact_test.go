package harness_test

import (
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/protocol"
)

func TestCompactReturnsWhatItFolded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.ownsLoop = false
		r := runtime(t, harness.NewMemStore(), f)
		s := create(t, r)
		keep := 1
		if got, err := s.Compact(bg, protocol.Compact{KeepTurns: &keep}); err != nil || got != (protocol.Compacted{}) {
			t.Fatalf("Compact with nothing to fold = %+v, %v, want folded false", got, err)
		}
		for _, id := range []string{"a", "b"} {
			submit(t, s, text(id, "hi"))
			run := <-f.runs
			run.emit(say("ok"))
			run.end()
		}
		type result struct {
			c   protocol.Compacted
			err error
		}
		done := make(chan result)
		go func() {
			c, err := s.Compact(bg, protocol.Compact{KeepTurns: &keep})
			done <- result{c, err}
		}()
		sum := <-f.runs
		sum.emit(say("summary"))
		sum.end()
		got := <-done
		if want := (protocol.Compacted{FromSeq: 1, ToSeq: 7, Folded: true}); got.err != nil || got.c != want {
			t.Fatalf("Compact = %+v, %v, want %+v", got.c, got.err, want)
		}
		closeRuntime(t, r)
	})
}
