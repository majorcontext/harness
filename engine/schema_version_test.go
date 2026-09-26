package engine

import (
	"reflect"
	"testing"

	"github.com/majorcontext/harness/message"
)

// TestSchemaVersionTracksShape: the whole point of deriving a cache version is
// that a field addition changes it without anyone remembering to. Adding a
// field, renaming its wire name, or changing its type must each produce a new
// version, and an unchanged shape must produce a stable one.
func TestSchemaVersionTracksShape(t *testing.T) {
	want := schemaVersion(reflect.TypeFor[struct {
		A int    `json:"a"`
		B string `json:"b"`
	}]())
	if got := schemaVersion(reflect.TypeFor[struct {
		A int    `json:"a"`
		B string `json:"b"`
	}]()); got != want {
		t.Errorf("schemaVersion is not stable: %d then %d", want, got)
	}
	if want <= 0 {
		t.Errorf("schemaVersion = %d, want a positive value (it is stored as a version int)", want)
	}
	for name, typ := range map[string]reflect.Type{
		"added field": reflect.TypeFor[struct {
			A int    `json:"a"`
			B string `json:"b"`
			C int    `json:"c"`
		}](),
		"renamed tag": reflect.TypeFor[struct {
			A int    `json:"a"`
			B string `json:"renamed"`
		}](),
		"changed type": reflect.TypeFor[struct {
			A int64  `json:"a"`
			B string `json:"b"`
		}](),
	} {
		if got := schemaVersion(typ); got == want {
			t.Errorf("%s: schemaVersion = %d, want a value different from the base shape", name, got)
		}
	}
}

// TestSchemaVersionTracksArrayLength: [4]T and [8]T are different wire
// shapes, but the array branch used to emit only the kind and the element
// type, so changing a field's array length left the digest unchanged and a
// snapshot with the wrong length was accepted.
func TestSchemaVersionTracksArrayLength(t *testing.T) {
	four := schemaVersion(reflect.TypeOf([4]int{}))
	eight := schemaVersion(reflect.TypeOf([8]int{}))
	if four == eight {
		t.Errorf("schemaVersion([4]int) = schemaVersion([8]int) = %d, want different digests for different array lengths", four)
	}
}

// TestSchemaVersionUnknownInterfacePanics: reflection cannot discover an
// interface's implementors, so a digest that met one it could not account
// for used to fall through to the default branch and hash only the kind,
// silently trusting a stale cache whose concrete variant changed shape. An
// interface schemaVersion has no registered variant list for must panic
// instead of under-specifying the shape.
func TestSchemaVersionUnknownInterfacePanics(t *testing.T) {
	type unregistered interface{ unregistered() }
	defer func() {
		if recover() == nil {
			t.Errorf("schemaVersion did not panic for an interface with no registered variants")
		}
	}()
	schemaVersion(reflect.TypeFor[unregistered]())
}

// TestSchemaVersionRegistersEveryPartVariant: a hand-maintained second list
// of message.Part's concrete types can drift from unmarshalPart's own
// dispatch, leaving a variant the wire decoder accepts but the digest never
// walks. interfaceVariants must derive from message.PartVariants, the same
// list unmarshalPart uses, not repeat it by hand.
func TestSchemaVersionRegistersEveryPartVariant(t *testing.T) {
	got, ok := interfaceVariants[reflect.TypeFor[message.Part]()]
	if !ok {
		t.Fatalf("message.Part has no registered variants")
	}
	want := message.PartVariants()
	if len(got) != len(want) {
		t.Fatalf("registered %d variants, message.PartVariants() has %d", len(got), len(want))
	}
	for i, v := range want {
		if got[i] != reflect.TypeOf(v) {
			t.Errorf("variant %d = %s, want %s (from message.PartVariants())", i, got[i], reflect.TypeOf(v))
		}
	}
}

// TestSchemaVersionTerminatesOnCycle: Session state reachable from a snapshot
// may be self-referential, and a shape digest that recursed forever would hang
// package init rather than fail a test.
func TestSchemaVersionTerminatesOnCycle(t *testing.T) {
	type node struct {
		Next *node `json:"next"`
		Leaf int   `json:"leaf"`
	}
	if got := schemaVersion(reflect.TypeFor[node]()); got <= 0 {
		t.Errorf("schemaVersion = %d, want a positive value for a cyclic shape", got)
	}
}

// TestCacheVersionsAreIndependent: the two caches previously shared a
// hand-bumped number each, and a field added to one silently left the other
// stale. Deriving each from its own struct is what decouples them.
func TestCacheVersionsAreIndependent(t *testing.T) {
	if sessionSnapshotVersion == sessionIndexVersion {
		t.Errorf("both cache versions = %d; each must track its own shape", sessionSnapshotVersion)
	}
	if sessionSnapshotVersion <= 0 || sessionIndexVersion <= 0 {
		t.Errorf("snapshot/index versions = %d/%d, want both positive", sessionSnapshotVersion, sessionIndexVersion)
	}
}
