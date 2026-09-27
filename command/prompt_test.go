package command

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestDiscoverRejectsSpecialFiles(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "review.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover([]string{root}); err == nil {
		t.Fatal("Discover accepted a non-regular command file")
	}
}

func TestDiscoverRejectsInvalidFrontmatterUTF8(t *testing.T) {
	root := t.TempDir()
	content := append([]byte("---\ndescription: "), 0xff)
	content = append(content, []byte("\n---\nbody")...)
	if err := os.WriteFile(filepath.Join(root, "review.md"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover([]string{root}); err == nil {
		t.Fatal("Discover advertised a command whose metadata is invalid UTF-8")
	}
	if _, err := LookupPrompt([]string{root}, "review"); err == nil {
		t.Fatal("LookupPrompt accepted invalid UTF-8 metadata")
	}
}

func TestPromptCommandRootSymlinkRejected(t *testing.T) {
	outside := t.TempDir()
	writePromptCommand(t, outside, "review.md", "---\ndescription: Outside\n---\noutside\n")
	root := filepath.Join(t.TempDir(), "commands")
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover([]string{root}); err == nil {
		t.Fatal("Discover followed a symlinked command root")
	}
	if _, err := LookupPrompt([]string{root}, "review"); err == nil {
		t.Fatal("LookupPrompt followed a symlinked command root")
	}
}

func TestDiscoverPromptCommands(t *testing.T) {
	root := t.TempDir()
	writePromptCommand(t, root, "review.md", "---\ndescription: Review changes\nargument-hint: <ref>\n---\nReview $1 and $ARGUMENTS\n")
	writePromptCommand(t, root, "git/sync.md", "---\ndescription: Sync changes\n---\nSync now\n")

	got, err := Discover([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "git:sync" || got[1].Name != "review" {
		t.Fatalf("names = %#v, want [git:sync review]", promptCommandNames(got))
	}
	if got[1].Description != "Review changes" || got[1].ArgHint != "<ref>" {
		t.Fatalf("metadata = %#v", got[1])
	}
	if !filepath.IsAbs(got[1].Path) {
		t.Fatalf("Path = %q, want absolute path", got[1].Path)
	}
	body, err := got[1].LoadBody()
	if err != nil {
		t.Fatal(err)
	}
	if body != "Review $1 and $ARGUMENTS\n" {
		t.Fatalf("body = %q", body)
	}
}

func TestDiscoverPromptCommandPrecedenceAndBuiltinCollision(t *testing.T) {
	user, project := t.TempDir(), t.TempDir()
	writePromptCommand(t, user, "review.md", "---\ndescription: User version\n---\nuser\n")
	writePromptCommand(t, project, "review.md", "---\ndescription: Project version\n---\nproject\n")
	writePromptCommand(t, project, "clear.md", "---\ndescription: Collision\n---\nno\n")

	got, err := Discover([]string{user, project})
	if err == nil || !strings.Contains(err.Error(), "clear") {
		t.Fatalf("Discover error = %v, want builtin collision for clear", err)
	}

	if err := os.Remove(filepath.Join(project, "clear.md")); err != nil {
		t.Fatal(err)
	}
	got, err = Discover([]string{user, project})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Description != "Project version" || got[0].Path != filepath.Join(project, "review.md") {
		t.Fatalf("winner = %#v, want project command", got)
	}
}

func TestDiscoverRejectsInvalidCommandMetadataAndNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string
	}{
		{name: "invalid name", file: "../bad.md"},
		{name: "missing description", file: "---\nargument-hint: <x>\n---\nbody"},
		{name: "unknown field", file: "---\ndescription: x\nname: other\n---\nbody"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.name == "invalid name" {
				if err := os.MkdirAll(filepath.Join(root, "Bad"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "Bad", "x.md"), []byte("---\ndescription: x\n---\nbody"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(filepath.Join(root, "bad.md"), []byte(tc.file), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Discover([]string{root}); err == nil {
				t.Fatal("Discover succeeded, want validation error")
			}
		})
	}
}

func TestLoadBodyRejectsInvalidOrEmptyBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{name: "invalid UTF-8", body: []byte{0xff}},
		{name: "empty", body: nil},
		{name: "whitespace only", body: []byte(" \n\t")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "review.md")
			contents := append([]byte("---\ndescription: Review\n---\n"), tc.body...)
			if err := os.WriteFile(path, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			commands, err := Discover([]string{root})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := commands[0].LoadBody(); err == nil {
				t.Fatal("LoadBody succeeded, want invalid or empty body error")
			}
		})
	}
}

func TestCommandDiscoveryRejectsNestedSymlinkPaths(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writePromptCommand(t, outside, "review.md", "---\ndescription: Review\n---\nbody\n")
	writePromptCommand(t, root, "nested/review.md", "---\ndescription: Review\n---\nbody\n")
	commands, err := Discover([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "nested"), filepath.Join(root, "original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "nested")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := LookupPrompt([]string{root}, "nested:review"); err == nil {
		t.Fatal("LookupPrompt followed a nested directory symlink")
	}
	if _, err := commands[0].LoadBody(); err == nil {
		t.Fatal("LoadBody followed a nested directory symlink after discovery")
	}
	if _, err := Discover([]string{root}); err == nil {
		t.Fatal("Discover accepted a nested directory symlink")
	}
}

func TestLookupPromptReadsOnlyNamedFile(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	writePromptCommand(t, first, "git/sync.md", "---\ndescription: Old\n---\nold\n")
	writePromptCommand(t, second, "git/sync.md", "---\ndescription: New\n---\nnew\n")
	got, err := LookupPrompt([]string{first, second}, "git:sync")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Description != "New" || got.Path != filepath.Join(second, "git", "sync.md") {
		t.Fatalf("LookupPrompt() = %#v, want later-directory command", got)
	}
	missing, err := LookupPrompt([]string{first}, "missing")
	if err != nil || missing != nil {
		t.Fatalf("missing lookup = (%#v, %v), want (nil, nil)", missing, err)
	}
	if _, err := LookupPrompt([]string{first}, "clear"); err == nil {
		t.Fatal("LookupPrompt accepted builtin alias clear")
	}
}

func TestLookupPromptInvalidNamesPassThrough(t *testing.T) {
	for _, name := range []string{"Cost", "foo.bar", "", "../review"} {
		got, err := LookupPrompt([]string{t.TempDir()}, name)
		if err != nil || got != nil {
			t.Errorf("LookupPrompt(%q) = (%#v, %v), want (nil, nil)", name, got, err)
		}
	}
}

func TestExpandPromptCommandArguments(t *testing.T) {
	got := Expand("$ARGUMENTS|$ARGUMENTS_EXTRA|$1|$2|$9|$0|${1}|$10", "one two three four five six seven eight nine ten")
	want := "one two three four five six seven eight nine ten|$ARGUMENTS_EXTRA|one|two|nine|$0|${1}|$10"
	if got != want {
		t.Fatalf("Expand() = %q, want %q", got, want)
	}
}

func writePromptCommand(t *testing.T, root, name, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func promptCommandNames(commands []*PromptCommand) []string {
	names := make([]string, len(commands))
	for i, c := range commands {
		names[i] = c.Name
	}
	return names
}
