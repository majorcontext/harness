package command

import (
	"sort"
	"testing"
)

// TestRegistryKindInvariants pins the spec's §1 rule: a control command
// names an Op, a frontend command names none. A violation means a client
// cannot tell from GET /commands whether the server or the client owns
// the action.
func TestRegistryKindInvariants(t *testing.T) {
	r := NewRegistry()
	for _, s := range r.All() {
		switch s.Kind {
		case KindControl:
			if s.Op == "" {
				t.Errorf("control command %q has no Op", s.Name)
			}
		case KindFrontend:
			if s.Op != "" {
				t.Errorf("frontend command %q has Op %q, want none", s.Name, s.Op)
			}
		default:
			t.Errorf("command %q has kind %q, want control or frontend", s.Name, s.Kind)
		}
	}
}

// TestRegistrySortedAndUnique pins that All() is menu-ready: sorted, and
// with no name or alias claimed twice. A duplicate would make Lookup's
// answer depend on table order.
func TestRegistrySortedAndUnique(t *testing.T) {
	r := NewRegistry()
	all := r.All()
	names := make([]string, len(all))
	for i, s := range all {
		names[i] = s.Name
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("All() not sorted: %v", names)
	}
	seen := map[string]bool{}
	for _, s := range all {
		for _, n := range append([]string{s.Name}, s.Aliases...) {
			if seen[n] {
				t.Errorf("name or alias %q claimed twice", n)
			}
			seen[n] = true
		}
	}
}

// TestAliasResolvesToSameSpec pins that /clear is the same entry as /new,
// not a copy that can drift.
func TestAliasResolvesToSameSpec(t *testing.T) {
	r := NewRegistry()
	byName, ok := r.Lookup("new")
	if !ok {
		t.Fatal(`Lookup("new") not found`)
	}
	byAlias, ok := r.Lookup("clear")
	if !ok {
		t.Fatal(`Lookup("clear") not found`)
	}
	if byName != byAlias {
		t.Errorf("Lookup(\"clear\") = %p, want the same *Spec as \"new\" (%p)", byAlias, byName)
	}
}

// TestDestructiveMarking pins that every command which loses context or a
// turn is marked, because a frontend gates confirmation on this field.
func TestDestructiveMarking(t *testing.T) {
	want := map[string]bool{"compact": true, "abort": true, "new": true, "queue-clear": true}
	r := NewRegistry()
	for name, wantDestructive := range want {
		s, ok := r.Lookup(name)
		if !ok {
			t.Errorf("Lookup(%q) not found", name)
			continue
		}
		if s.Destructive != wantDestructive {
			t.Errorf("%q Destructive = %v, want %v", name, s.Destructive, wantDestructive)
		}
	}
}
