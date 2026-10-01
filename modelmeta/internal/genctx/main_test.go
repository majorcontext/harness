package main

import (
	"bytes"
	"flag"
	"os"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden file")

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestGenerateGolden pins the whole output for a fixed catalog: sorted keys,
// zero-context exclusion, bedrock key normalization with the smallest window
// winning a collision, last-segment fireworks keys, Gemini-only vertex keys,
// and an override replacing a models.dev value.
func TestGenerateGolden(t *testing.T) {
	code, notes, err := generate(readFile(t, "testdata/catalog.json"), readFile(t, "testdata/overrides.json"))
	if err != nil {
		t.Fatal(err)
	}
	const golden = "testdata/context_windows_gen.golden"
	if *update {
		if err := os.WriteFile(golden, code, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if want := readFile(t, golden); !bytes.Equal(code, want) {
		t.Errorf("generated output differs from %s; rerun with -update and review the diff\ngot:\n%s", golden, code)
	}
	wantNotes := []string{
		`bifrostFireworksContextWindows["open-b"] override repeats models.dev value 512000`,
		`openaiContextWindows["gpt-x"] override 300000 shadows models.dev value 400000`,
	}
	if strings.Join(notes, "\n") != strings.Join(wantNotes, "\n") {
		t.Errorf("notes = %q; want %q", notes, wantNotes)
	}
}

func TestGenerateRejectsUnknownOverrideTable(t *testing.T) {
	_, _, err := generate(readFile(t, "testdata/catalog.json"), []byte(`{"nopeContextWindows":{"m":{"context":1}}}`))
	if err == nil {
		t.Fatal("generate accepted an override for an unknown table")
	}
}

func TestGenerateRejectsMissingProvider(t *testing.T) {
	_, _, err := generate([]byte(`{}`), []byte(`{}`))
	if err == nil {
		t.Fatal("generate accepted a catalog with no providers")
	}
}
