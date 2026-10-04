package migrate

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"

	"github.com/majorcontext/harness"
)

// Archive converts the engine journals in a box archive. r is a
// sessions.tar.zst export, and w receives the same archive with the
// converted logs added. dir is the session directory of the engine and
// store is the root of the DiskStore of the logs, both relative to the
// archive root. Every entry of r is in w unchanged.
func Archive(ctx context.Context, r io.Reader, w io.Writer, dir, store string) ([]Result, error) {
	if !filepath.IsLocal(dir) || !filepath.IsLocal(store) {
		return nil, fmt.Errorf("migrate: archive paths %q and %q must be local", dir, store)
	}
	tmp, err := os.MkdirTemp("", "harness-migrate-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := extract(r, tmp); err != nil {
		return nil, err
	}
	results, err := Dir(ctx, filepath.Join(tmp, dir), harness.NewDiskStore(filepath.Join(tmp, store)))
	if err != nil {
		return nil, err
	}
	return results, pack(tmp, w)
}

func extract(r io.Reader, root string) error {
	zr, err := zstd.NewReader(r)
	if err != nil {
		return err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("migrate: read archive: %w", err)
		}
		name := filepath.FromSlash(h.Name)
		if !filepath.IsLocal(name) {
			return fmt.Errorf("migrate: archive entry %q is not local", h.Name)
		}
		path := filepath.Join(root, name)
		switch h.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(path, 0o755)
		case tar.TypeReg:
			err = writeFile(path, tr, h)
		default:
			err = fmt.Errorf("migrate: archive entry %q has type %q", h.Name, h.Typeflag)
		}
		if err != nil {
			return err
		}
	}
}

func writeFile(path string, r io.Reader, h *tar.Header) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fs.FileMode(h.Mode).Perm())
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Chtimes(path, h.ModTime, h.ModTime)
}

func pack(root string, w io.Writer) error {
	zw, err := zstd.NewWriter(w)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(zw)
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == root {
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
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			h.Name += "/"
		}
		if err := tw.WriteHeader(h); err != nil || !d.Type().IsRegular() {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.Copy(tw, f)
		return err
	})
	if err == nil {
		err = tw.Close()
	}
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	return err
}
