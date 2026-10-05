package migrate

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/message"
)

// boxOwner is the owner of each entry of a recorded export.
var boxOwner = tar.Header{Uid: 1000, Gid: 1000, Uname: "box", Gname: "box"}

// exportArchive returns the files under root as tar --zstd -C <disk>/harness
// sessions writes them, with the owner of the box and an extended attribute.
func exportArchive(t *testing.T, root string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw, err := zstd.NewWriter(&b)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == root {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		h := boxOwner
		h.Name, h.ModTime, h.Format = filepath.ToSlash(rel), at, tar.FormatPAX
		h.PAXRecords = map[string]string{"SCHILY.xattr.user.box": "1"}
		var data []byte
		if d.IsDir() {
			h.Typeflag, h.Name, h.Mode = tar.TypeDir, h.Name+"/", 0o750
		} else {
			if data, err = os.ReadFile(path); err != nil {
				return err
			}
			h.Typeflag, h.Mode, h.Size = tar.TypeReg, 0o640, int64(len(data))
		}
		if err := tw.WriteHeader(&h); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func headers(t *testing.T, archive []byte) []*tar.Header {
	t.Helper()
	zr, err := zstd.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	var out []*tar.Header
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, h)
	}
}

func TestArchiveAddsTheConvertedLogs(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.CopyFS(filepath.Join(root, "sessions"), os.DirFS("testdata/journals")); err != nil {
		t.Fatal(err)
	}
	in := exportArchive(t, root)
	var out bytes.Buffer
	results, err := Archive(ctx, bytes.NewReader(in), &out, "sessions", "sessions", message.ModelRef{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 12 || len(Failed(results)) != 1 {
		t.Fatalf("results %+v", results)
	}
	old, all := headers(t, in), headers(t, out.Bytes())
	for i, h := range old {
		g := all[i]
		if g.Name != h.Name || g.Uid != h.Uid || g.Uname != h.Uname || g.Mode != h.Mode || !g.ModTime.Equal(h.ModTime) || g.PAXRecords["SCHILY.xattr.user.box"] != "1" {
			t.Errorf("entry %d is %+v, want %+v", i, g, h)
		}
	}
	for _, g := range all[len(old):] {
		if !strings.HasPrefix(g.Name, "sessions/ses_") || g.Uid != boxOwner.Uid || g.Gid != boxOwner.Gid || g.Uname != boxOwner.Uname || g.Gname != boxOwner.Gname {
			t.Errorf("added entry %q has owner %d:%d %s:%s, want the owner of the session directory", g.Name, g.Uid, g.Gid, g.Uname, g.Gname)
		}
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
	var again bytes.Buffer
	results, err = Archive(ctx, bytes.NewReader(out.Bytes()), &again, "sessions", "sessions", message.ModelRef{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Session != "ses_0000000000000010" && !r.Skipped {
			t.Errorf("a second run did not skip %s: %+v", r.Session, r)
		}
	}
	if n, want := len(headers(t, again.Bytes())), len(all); n != want {
		t.Errorf("a second run wrote %d entries, want the %d of its input", n, want)
	}
}

// extract writes the entries of a sessions.tar.zst archive under root.
func extract(r io.Reader, root string) error {
	c := copier{root: root, seen: map[string]bool{}}
	return c.copy(r, tar.NewWriter(io.Discard))
}
