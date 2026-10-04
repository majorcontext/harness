package migrate

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/message"
)

// Archive converts the engine journals in a box archive. r is a
// sessions.tar.zst export. w receives each entry of r with its header and
// bytes unchanged, then an entry for each file that the conversion adds,
// with the owner of the session directory. dir is the session directory of
// the engine and store is the root of the DiskStore of the logs, both
// relative to the archive root. model is as for Dir. After an error, w can
// hold part of an archive, so a caller writes to a temporary object and
// replaces the archive only when Archive returns no error.
func Archive(ctx context.Context, r io.Reader, w io.Writer, dir, store string, model message.ModelRef) ([]Result, error) {
	if !filepath.IsLocal(dir) || !filepath.IsLocal(store) {
		return nil, fmt.Errorf("migrate: archive paths %q and %q must be local", dir, store)
	}
	tmp, err := os.MkdirTemp("", "harness-migrate-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	zw, err := zstd.NewWriter(w)
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(zw)
	c := copier{root: tmp, under: []string{path.Clean(filepath.ToSlash(dir)), path.Clean(filepath.ToSlash(store))}, seen: map[string]bool{}}
	var results []Result
	err = c.copy(r, tw)
	if err == nil {
		results, err = Dir(ctx, filepath.Join(tmp, dir), harness.NewDiskStore(filepath.Join(tmp, store)), model)
	}
	if err == nil {
		err = c.add(tw, filepath.Join(tmp, store))
	}
	if err == nil {
		err = tw.Close()
	}
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	return results, nil
}

// copier copies the entries of an archive, and extracts each entry under
// one of the directories under to root. With no directories, it extracts
// each entry.
type copier struct {
	root  string
	under []string
	seen  map[string]bool
	// owner is the header of the first extracted entry.
	owner *tar.Header
}

func (c *copier) copy(r io.Reader, tw *tar.Writer) error {
	zr, err := zstd.NewReader(r)
	if err != nil {
		return err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("migrate: read archive: %w", err)
		}
		if err := c.entry(tr, tw, h); err != nil {
			return err
		}
	}
}

// entry copies the entry h, whose bytes tr reads, to tw.
func (c *copier) entry(tr io.Reader, tw *tar.Writer, h *tar.Header) error {
	name := path.Clean(strings.TrimPrefix(h.Name, "./"))
	if !filepath.IsLocal(filepath.FromSlash(name)) {
		return fmt.Errorf("migrate: archive entry %q is not local", h.Name)
	}
	c.seen[name] = true
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	var f *os.File
	if c.extracted(name) {
		var err error
		if f, err = c.extract(name, h); err != nil {
			return err
		}
	}
	if f == nil {
		_, err := io.Copy(tw, tr)
		return err
	}
	_, err := io.Copy(tw, io.TeeReader(tr, f))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (c *copier) extracted(name string) bool {
	if len(c.under) == 0 {
		return true
	}
	for _, d := range c.under {
		if name == d || strings.HasPrefix(name, d+"/") {
			return true
		}
	}
	return false
}

// extract makes the directory of a directory entry, or returns the file of
// a regular entry.
func (c *copier) extract(name string, h *tar.Header) (*os.File, error) {
	if c.owner == nil {
		c.owner = &tar.Header{Uid: h.Uid, Gid: h.Gid, Uname: h.Uname, Gname: h.Gname}
	}
	p := filepath.Join(c.root, filepath.FromSlash(name))
	switch h.Typeflag {
	case tar.TypeDir:
		return nil, os.MkdirAll(p, 0o755)
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, err
		}
		return os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	}
	return nil, fmt.Errorf("migrate: archive entry %q has type %q", h.Name, h.Typeflag)
}

// add writes an entry for each file and directory under store that the
// archive does not hold.
func (c *copier) add(tw *tar.Writer, store string) error {
	err := filepath.WalkDir(store, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(c.root, p)
		if err != nil || c.seen[filepath.ToSlash(rel)] {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		h, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			h.Name += "/"
		}
		if c.owner != nil {
			h.Uid, h.Gid, h.Uname, h.Gname = c.owner.Uid, c.owner.Gid, c.owner.Uname, c.owner.Gname
		}
		if err := tw.WriteHeader(h); err != nil || !d.Type().IsRegular() {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.Copy(tw, f)
		return err
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
