package harness_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/storetest"
)

func TestDiskStore(t *testing.T) {
	storetest.Run(t, func(t *testing.T) func() harness.Store {
		root := t.TempDir()
		return func() harness.Store { return harness.NewDiskStore(root) }
	})
}

func TestDiskStoreRepairsTornTail(t *testing.T) {
	root, ctx := t.TempDir(), context.Background()
	log := writeLog(t, root, "s", "a\nb\ntor")
	st := harness.NewDiskStore(root)
	if head, err := st.Head(ctx, "s"); head != 2 || err != nil {
		t.Fatalf("Head = %d, %v, want 2, nil", head, err)
	}
	if err := st.Append(ctx, "s", 2, []byte("c")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(log); string(got) != "a\nb\nc\n" {
		t.Fatalf("log = %q, want a, b, c lines", got)
	}
}

func writeLog(t *testing.T, root, session, content string) string {
	t.Helper()
	if err := os.Mkdir(filepath.Join(root, session), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(root, session, "log.jsonl")
	if err := os.WriteFile(log, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return log
}

func TestDiskStoreReadsLeaveTornTail(t *testing.T) {
	root, ctx := t.TempDir(), context.Background()
	log := writeLog(t, root, "s", "a\nb\ntor")
	st := harness.NewDiskStore(root)
	got, err := st.Read(ctx, "s", 0, 10)
	if err != nil || len(got) != 2 || string(got[1].Data) != "b" {
		t.Fatalf("Read = %v, %v, want records a and b", got, err)
	}
	if head, err := st.Head(ctx, "s"); head != 2 || err != nil {
		t.Fatalf("Head = %d, %v, want 2, nil", head, err)
	}
	if b, _ := os.ReadFile(log); string(b) != "a\nb\ntor" {
		t.Fatalf("log = %q after reads, want it unchanged", b)
	}
}

func TestDiskStoreSessionsSkipTornOnlyLog(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "torn", "tor")
	writeLog(t, root, "whole", "a\n")
	got, err := harness.NewDiskStore(root).Sessions(context.Background(), "", 10)
	if err != nil || len(got) != 1 || got[0] != "whole" {
		t.Fatalf("Sessions = %q, %v, want [whole]", got, err)
	}
}

func TestDiskStoreReopenContinuesSeq(t *testing.T) {
	root, ctx := t.TempDir(), context.Background()
	if err := harness.NewDiskStore(root).Append(ctx, "s", 0, []byte("a"), []byte("b")); err != nil {
		t.Fatal(err)
	}
	st := harness.NewDiskStore(root)
	if err := st.Append(ctx, "s", 2, []byte("c")); err != nil {
		t.Fatal(err)
	}
	got, err := st.Read(ctx, "s", 1, 10)
	if err != nil || len(got) != 2 || got[1].Seq != 3 || string(got[1].Data) != "c" {
		t.Fatalf("Read = %v, %v, want records 2 and 3", got, err)
	}
}

func TestDiskStoreRejectsUnsafeNames(t *testing.T) {
	st, ctx := harness.NewDiskStore(t.TempDir()), context.Background()
	for _, name := range []string{"", ".", "..", "a/b", `a\b`, "../x"} {
		if err := st.Append(ctx, name, 0, []byte("r")); err == nil {
			t.Errorf("Append to session %q succeeded", name)
		}
		if err := st.PutBlob(ctx, "s", name, strings.NewReader("b")); err == nil {
			t.Errorf("PutBlob with key %q succeeded", name)
		}
		if _, err := st.GetBlob(ctx, "s", name); err == nil {
			t.Errorf("GetBlob with key %q succeeded", name)
		}
	}
}
