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

// TestGenerateInfoGolden pins the model facts for a fixed catalog: the removal of the latest suffix,
// the listed flag for a deprecated model and a model without tool calls, the mapping of none, toggle,
// and budget_tokens to the levels of harness, the levels of a model with reasoning and no usable
// option, an empty list for a model without reasoning, and the provider display names.
func TestGenerateInfoGolden(t *testing.T) {
	code, _, err := generateInfo(readFile(t, "testdata/catalog.json"), readFile(t, "testdata/overrides.json"))
	if err != nil {
		t.Fatal(err)
	}
	const golden = "testdata/model_info_gen.golden"
	if *update {
		if err := os.WriteFile(golden, code, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if want := readFile(t, golden); !bytes.Equal(code, want) {
		t.Errorf("generated output differs from %s; rerun with -update and review the diff\ngot:\n%s", golden, code)
	}
}

func TestGenerateRejectsUnknownOffType(t *testing.T) {
	ovr := `{"anthropicThinkingCanBeDisabled":{"claude-a-4":{"offType":"enabled"}}}`
	if _, _, err := generateInfo(readFile(t, "testdata/catalog.json"), []byte(ovr)); err == nil {
		t.Fatal("generateInfo accepted an unknown offType")
	}
}

func TestGenerateRejectsOffTypeOutsideThinkingDisableTable(t *testing.T) {
	ovr := `{"thinkingOffByDefault":{"claude-off":{"offType":"between_tools"}}}`
	if _, _, err := generate(readFile(t, "testdata/catalog.json"), []byte(ovr)); err == nil {
		t.Fatal("generate accepted offType in a table that is not a thinking-disable table")
	}
}
