package acp

import (
	"slices"
	"testing"
)

func TestHeldSendsQueueUntilReleaseThenKeepOrder(t *testing.T) {
	var got []string
	send := func(method string, params any) error {
		got = append(got, method+":"+params.(string))
		return nil
	}
	held, release := holdSends(send)
	_ = held("u", "delta-1")
	_ = held("u", "delta-2")
	if len(got) != 0 {
		t.Fatalf("sent %v before release, want none", got)
	}
	release()
	_ = held("u", "delta-3")
	want := []string{"u:delta-1", "u:delta-2", "u:delta-3"}
	if !slices.Equal(got, want) {
		t.Errorf("sent %v, want %v: every queued update first, in order, then the later ones", got, want)
	}
}
