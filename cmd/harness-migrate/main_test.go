package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

const journal = `{"type":"session","id":"ses_0000000000000001","created_at":"2026-09-08T07:00:00Z","model":"openai/gpt-5"}
{"type":"message","message":{"id":"msg_1","role":"user","parts":[{"type":"text","text":"hi"}]}}
{"type":"message","message":{"id":"msg_2","role":"assistant","parts":[{"type":"text","text":"hello"}]}}
`

func archiveOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw, err := zstd.NewWriter(&b)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	for name, data := range files {
		h := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o640, Size: int64(len(data))}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func entryNames(t *testing.T, archive []byte) []string {
	t.Helper()
	zr, err := zstd.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
}

func TestRunFailsWhenFromDoesNotExist(t *testing.T) {
	var out bytes.Buffer
	err := run(context.Background(), []string{"-from", filepath.Join(t.TempDir(), "typo"), "-to", t.TempDir()}, &out)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want the missing directory", err)
	}
}

func TestRunConvertsADirectory(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(from, "ses_0000000000000001.jsonl"), []byte(journal), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-from", from, "-to", to}, &out); err != nil {
		t.Fatal(err)
	}
	if want := "converted ses_0000000000000001: 2 messages\n"; out.String() != want {
		t.Errorf("out %q, want %q", out.String(), want)
	}
}

func TestConvertArchiveReplacesTheArchiveOnlyWhole(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "sessions.tar.zst")
	in := archiveOf(t, map[string]string{"sessions/ses_0000000000000001.jsonl": journal})
	if err := os.WriteFile(src, in, 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out.tar.zst")
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-archive", src, "-out", dst}, &out); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	sum := md5.Sum(got)
	want := fmt.Sprintf("archive %s: size_bytes %d checksum_md5_base64 %s\n", dst, len(got), base64.StdEncoding.EncodeToString(sum[:]))
	if !strings.HasPrefix(out.String(), want) || !strings.Contains(out.String(), "converted ses_0000000000000001: 2 messages") {
		t.Errorf("out %q, want it to start with %q and report the session", out.String(), want)
	}
	if info, err := os.Stat(dst); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, %v, want the mode of the source", info, err)
	}
	names := entryNames(t, got)
	if !slices.Contains(names, "sessions/ses_0000000000000001.jsonl") || !slices.ContainsFunc(names, func(n string) bool { return strings.HasPrefix(n, "sessions/ses_0000000000000001/") }) {
		t.Errorf("entries %v lack the journal or its converted log", names)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".harness-migrate-*")); len(left) != 0 {
		t.Errorf("temporary files %v", left)
	}

	bad := filepath.Join(dir, "bad.tar.zst")
	if err := os.WriteFile(bad, []byte("not an archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "keep.tar.zst")
	if err := os.WriteFile(keep, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"-archive", bad, "-out", keep}, &out); err == nil {
		t.Fatal("a bad archive converted")
	}
	if b, _ := os.ReadFile(keep); string(b) != "previous" {
		t.Errorf("a failed conversion changed the output to %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".harness-migrate-*")); len(left) != 0 {
		t.Errorf("temporary files %v after a failure", left)
	}
}
