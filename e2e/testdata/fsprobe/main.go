//go:build js

// Command fsprobe runs inside the browser Worker of the browser job, on the file
// system of sdk/embed, and prints one line for each check: "ok <name>" or
// "FAIL <name>: <detail>". The arguments are the directories to run the checks
// in: one in memory and one in the persistent store.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

func check(name string, err error) {
	if err != nil {
		fmt.Printf("FAIL %s: %v\n", name, err)
		return
	}
	fmt.Printf("ok %s\n", name)
}

func want(cond bool, format string, args ...any) error {
	if cond {
		return nil
	}
	return fmt.Errorf(format, args...)
}

func main() {
	if st, err := os.Stdout.Stat(); err != nil {
		check("stdout is a character device", err)
	} else {
		check("stdout is a character device", want(st.Mode()&os.ModeCharDevice != 0, "mode %v", st.Mode()))
	}
	for _, dir := range os.Args[1:] {
		suite(dir)
	}
	check("a relative path resolves against the work directory", relative())
	os.Exit(0)
}

func relative() error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := os.WriteFile("rel.txt", []byte("r"), 0o644); err != nil {
		return err
	}
	defer os.Remove(filepath.Join(wd, "rel.txt"))
	b, err := os.ReadFile(filepath.Join(wd, "rel.txt"))
	if err != nil {
		return err
	}
	return want(string(b) == "r" && wd == "/work", "wd %q content %q", wd, b)
}

func suite(dir string) {
	tag := dir + ": "
	check(tag+"mkdir all", os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755))
	check(tag+"mkdir of an existing dir is exist", func() error {
		return want(errors.Is(os.Mkdir(dir, 0o755), fs.ErrExist), "not ErrExist")
	}())
	check(tag+"open of a missing file is not exist", func() error {
		_, err := os.Open(filepath.Join(dir, "missing"))
		return want(errors.Is(err, fs.ErrNotExist), "err %v", err)
	}())
	check(tag+"a path through a file is not a directory", func() error {
		f := filepath.Join(dir, "plain")
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			return err
		}
		_, err := os.Stat(filepath.Join(f, "x"))
		return want(errors.Is(err, syscall.ENOTDIR), "err %v", err)
	}())
	check(tag+"open of a directory for write is a directory error", func() error {
		_, err := os.OpenFile(filepath.Join(dir, "a"), os.O_WRONLY, 0)
		return want(errors.Is(err, syscall.EISDIR), "err %v", err)
	}())
	check(tag+"remove of a non-empty directory fails", func() error {
		err := os.Remove(filepath.Join(dir, "a"))
		return want(errors.Is(err, syscall.ENOTEMPTY), "err %v", err)
	}())
	check(tag+"exclusive create of an existing file fails", func() error {
		_, err := os.OpenFile(filepath.Join(dir, "plain"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		return want(errors.Is(err, fs.ErrExist), "err %v", err)
	}())
	check(tag+"append, truncate, and stat", appendTruncate(dir))
	check(tag+"partial reads, seek, and ReadAt", reads(dir))
	check(tag+"two descriptors of one file", twoDescriptors(dir))
	check(tag+"rename over an existing file", renameOver(dir))
	check(tag+"rename onto a destination that a reader holds open", renameOntoOpen(dir))
	check(tag+"concurrent exclusive creates of one path give one winner", exclusiveRace(dir))
	check(tag+"readdir", readdir(dir))
	check(tag+"truncate by path and by file", truncates(dir))
	check(tag+"temporary file, sync, rename", temp(dir))
	check(tag+"chmod, chtimes, and symlink do not panic", noops(dir))
	check(tag+"a large write reads back", large(dir))
	check(tag+"remove all", os.RemoveAll(filepath.Join(dir, "a")))
	metric(dir)
}

func appendTruncate(dir string) error {
	p := filepath.Join(dir, "log")
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	for _, s := range []string{"one\n", "two\n"} {
		if _, err := f.WriteString(s); err != nil {
			return err
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	b, _ := os.ReadFile(p)
	if string(b) != "one\ntwo\n" {
		return fmt.Errorf("after append %q", b)
	}
	st, err := os.Stat(p)
	if err != nil {
		return err
	}
	if st.Size() != 8 || st.Mode().Perm() == 0 || time.Since(st.ModTime()) > time.Hour || st.IsDir() {
		return fmt.Errorf("stat size %d mode %v mtime %v", st.Size(), st.Mode(), st.ModTime())
	}
	g, err := os.OpenFile(p, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	_, _ = g.WriteString("x")
	_ = g.Close()
	b, _ = os.ReadFile(p)
	return want(string(b) == "x", "after truncate %q", b)
}

func reads(dir string) error {
	p := filepath.Join(dir, "ten")
	if err := os.WriteFile(p, []byte("0123456789"), 0o644); err != nil {
		return err
	}
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 4)
	var got []string
	for {
		n, err := f.Read(buf)
		got = append(got, string(buf[:n]))
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if strings.Join(got, "|") != "0123|4567|89|" {
		return fmt.Errorf("sequential reads %v", got)
	}
	if _, err := f.Seek(3, io.SeekStart); err != nil {
		return err
	}
	n, _ := f.Read(buf)
	if string(buf[:n]) != "3456" {
		return fmt.Errorf("after seek %q", buf[:n])
	}
	n, err = f.ReadAt(buf, 8)
	if n != 2 || err != io.EOF || string(buf[:n]) != "89" {
		return fmt.Errorf("ReadAt %d %v %q", n, err, buf[:n])
	}
	w, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer w.Close()
	if _, err := w.WriteAt([]byte("AB"), 4); err != nil {
		return err
	}
	b, _ := os.ReadFile(p)
	return want(string(b) == "0123AB6789", "after WriteAt %q", b)
}

func twoDescriptors(dir string) error {
	p := filepath.Join(dir, "shared")
	w, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer w.Close()
	if _, err := w.WriteString("abc"); err != nil {
		return err
	}
	r, err := os.Open(p)
	if err != nil {
		return fmt.Errorf("second open: %w", err)
	}
	b, _ := io.ReadAll(r)
	_ = r.Close()
	if string(b) != "abc" {
		return fmt.Errorf("second descriptor read %q", b)
	}
	_, err = w.WriteString("def")
	return err
}

func renameOver(dir string) error {
	a, b := filepath.Join(dir, "ra"), filepath.Join(dir, "rb")
	if err := os.WriteFile(a, []byte("new"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(b, []byte("old"), 0o644); err != nil {
		return err
	}
	if err := os.Rename(a, b); err != nil {
		return err
	}
	got, _ := os.ReadFile(b)
	_, err := os.Stat(a)
	return want(string(got) == "new" && errors.Is(err, fs.ErrNotExist), "dest %q, source err %v", got, err)
}

func renameOntoOpen(dir string) error {
	dst, src := filepath.Join(dir, "open-dst"), filepath.Join(dir, "open-src")
	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		return err
	}
	r, err := os.Open(dst)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := os.WriteFile(src, []byte("new content"), 0o644); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "new content" {
		return fmt.Errorf("destination %q %v", got, err)
	}
	_, err = os.Stat(src)
	return want(errors.Is(err, fs.ErrNotExist), "source still exists: %v", err)
}

func exclusiveRace(dir string) error {
	p := filepath.Join(dir, "excl")
	const n = 8
	results := make(chan error, n)
	for range n {
		go func() {
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err == nil {
				_ = f.Close()
			}
			results <- err
		}()
	}
	won := 0
	for range n {
		if err := <-results; err == nil {
			won++
		} else if !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	return want(won == 1, "%d creates succeeded, want 1", won)
}

func readdir(dir string) error {
	d := filepath.Join(dir, "ls")
	if err := os.MkdirAll(filepath.Join(d, "sub"), 0o755); err != nil {
		return err
	}
	for _, n := range []string{"b", "a"} {
		if err := os.WriteFile(filepath.Join(d, n), nil, 0o644); err != nil {
			return err
		}
	}
	es, err := os.ReadDir(d)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range es {
		names = append(names, fmt.Sprintf("%s:%v", e.Name(), e.IsDir()))
	}
	if !slices.Equal(names, []string{"a:false", "b:false", "sub:true"}) {
		return fmt.Errorf("entries %v", names)
	}
	_, err = os.ReadDir(filepath.Join(d, "a"))
	return want(err != nil, "ReadDir of a file succeeded")
}

func truncates(dir string) error {
	p := filepath.Join(dir, "tr")
	if err := os.WriteFile(p, []byte("0123456789"), 0o644); err != nil {
		return err
	}
	if err := os.Truncate(p, 4); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(2); err != nil {
		return err
	}
	st, _ := f.Stat()
	if st.Size() != 2 {
		return fmt.Errorf("size %d", st.Size())
	}
	return os.Truncate(p, 6)
}

func temp(dir string) error {
	f, err := os.CreateTemp(dir, ".put-*")
	if err != nil {
		return err
	}
	if _, err := f.WriteString("blob"); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	dst := filepath.Join(dir, "blob")
	if err := os.Rename(f.Name(), dst); err != nil {
		return err
	}
	b, err := os.ReadFile(dst)
	if err != nil || string(b) != "blob" {
		return fmt.Errorf("blob %q %v", b, err)
	}
	return nil
}

func noops(dir string) error {
	p := filepath.Join(dir, "plain")
	if err := os.Chmod(p, 0o600); err != nil {
		return err
	}
	if err := os.Chtimes(p, time.Now(), time.Now()); err != nil {
		return err
	}
	if err := os.Symlink(p, filepath.Join(dir, "link")); err == nil {
		return errors.New("symlink succeeded")
	}
	return nil
}

func large(dir string) error {
	p := filepath.Join(dir, "large")
	data := bytes.Repeat([]byte("0123456789abcdef"), 1<<16)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	return want(bytes.Equal(b, data), "%d bytes read, %d written", len(b), len(data))
}

// metric prints the cost of a record append: one write of 200 bytes and one fsync.
func metric(dir string) {
	f, err := os.OpenFile(filepath.Join(dir, "metric"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		check("metric", err)
		return
	}
	defer f.Close()
	rec := append(bytes.Repeat([]byte("r"), 199), '\n')
	const n = 200
	start := time.Now()
	for range n {
		if _, err := f.Write(rec); err != nil {
			check("metric", err)
			return
		}
		if err := f.Sync(); err != nil {
			check("metric", err)
			return
		}
	}
	fmt.Printf("metric %s appendSyncMicros %d\n", dir, time.Since(start).Microseconds()/n)
}
