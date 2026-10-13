package main

import (
	"io"
	"strings"
	"testing"
)

func TestParentWhoseSubtestsAllSkipIsNotALeaf(t *testing.T) {
	stream := strings.Join([]string{
		`{"Action":"skip","Package":"example.com/p","Test":"TestA/one","Elapsed":0}`,
		`{"Action":"skip","Package":"example.com/p","Test":"TestA/two","Elapsed":0}`,
		`{"Action":"pass","Package":"example.com/p","Test":"TestA","Elapsed":99}`,
		`{"Action":"pass","Package":"example.com/p","Test":"TestB","Elapsed":99}`,
	}, "\n")
	rep := consume(strings.NewReader(stream), io.Discard)
	b := &budget{leaf: 10, pkgBase: 10, pkgs: map[string]float64{}}
	over := rep.check(b, "")
	if len(over) != 1 || !strings.Contains(over[0], "TestB") {
		t.Fatalf("over = %q, want only TestB", over)
	}
}
