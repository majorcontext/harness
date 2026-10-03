package harness

import (
	"context"
	"errors"
	"testing"
)

func TestLocalOwnerGrantsOneAtATime(t *testing.T) {
	ctx := context.Background()
	o := newLocalOwner()
	g, err := o.Acquire(ctx, "s1")
	if err != nil || g.Epoch() != 1 {
		t.Fatalf("Acquire = %v, %v; want epoch 1", g, err)
	}
	if _, err := o.Acquire(ctx, "s1"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second Acquire = %v, want ErrBusy", err)
	}
	if _, err := o.Acquire(ctx, "s2"); err != nil {
		t.Fatalf("Acquire of another session = %v", err)
	}
	g.Release()
	if _, err := o.Acquire(ctx, "s1"); err != nil {
		t.Fatalf("Acquire after Release = %v", err)
	}
}
