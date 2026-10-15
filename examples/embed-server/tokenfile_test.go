package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTokenFileKeepsLoginsAcrossRestarts(t *testing.T) {
	file := filepath.Join(t.TempDir(), "tokens.json")
	a := newTokenStore("", nil)
	a.file = file
	a.Set("ann", tokens{Access: "access-a", Refresh: "refresh-a", Account: "acct-a"})
	a.Set("bob", tokens{Access: "access-b", Refresh: "refresh-b", Account: "acct-b"})
	a.Delete("bob")

	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode %o, want 600", mode)
	}

	b := newTokenStore("", nil)
	b.file = file
	if err := b.load(); err != nil {
		t.Fatal(err)
	}
	if !b.Has("ann") || b.Has("bob") {
		t.Fatalf("after load: ann %v bob %v, want true false", b.Has("ann"), b.Has("bob"))
	}
	if got := b.entry("ann").t; got != (tokens{Access: "access-a", Refresh: "refresh-a", Account: "acct-a"}) {
		t.Errorf("ann has a login that differs from the saved one")
	}
}

func TestNoTokenFileWritesNothing(t *testing.T) {
	dir := t.TempDir()
	s := newTokenStore("", nil)
	s.Set("ann", tokens{Access: "a"})
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %d files", len(entries))
	}
	if err := s.load(); err != nil {
		t.Fatal(err)
	}
}
