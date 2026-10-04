package migrate

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/majorcontext/harness"
)

// An archive export is tar --zstd -C <disk>/harness sessions.
func exportArchive(t *testing.T, root string) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := pack(root, &b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestArchiveAddsTheConvertedLogs(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.CopyFS(filepath.Join(root, "sessions"), os.DirFS("testdata/journals")); err != nil {
		t.Fatal(err)
	}
	in := exportArchive(t, root)
	var out bytes.Buffer
	results, err := Archive(ctx, bytes.NewReader(in), &out, "sessions", "sessions")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 8 || len(Failed(results)) != 1 {
		t.Fatalf("results %+v", results)
	}
	got := t.TempDir()
	if err := extract(bytes.NewReader(out.Bytes()), got); err != nil {
		t.Fatal(err)
	}
	if err := sameTree(filepath.Join(got, "sessions"), "testdata/journals"); err != nil {
		t.Error(err)
	}
	id := "ses_000000000000000b"
	s := replay(t, harness.NewDiskStore(filepath.Join(got, "sessions")), id)
	if want := oldTranscript(t, filepath.Join(got, "sessions"), id); !slices.Equal(newTranscript(s), want) {
		t.Errorf("archived history %q, want %q", newTranscript(s), want)
	}
	again, err := Archive(ctx, bytes.NewReader(out.Bytes()), &bytes.Buffer{}, "sessions", "sessions")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range again {
		if r.Session != "ses_0000000000000010" && !r.Skipped {
			t.Errorf("a second run did not skip %s: %+v", r.Session, r)
		}
	}
}
