package builtin

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // register the GIF decoder for image.DecodeConfig
	_ "image/jpeg" // register the JPEG decoder for image.DecodeConfig
	_ "image/png"  // register the PNG decoder for image.DecodeConfig
	"io"
	"io/fs"
	"net/http"
	"os"

	_ "golang.org/x/image/webp" // register the WebP decoder for image.DecodeConfig
)

// maxFileBytes bounds each read of a file. The cap binds on the bytes
// read, never on a stat that a growing file could outrun.
const maxFileBytes = 20 << 20

var errTooLarge = fmt.Errorf("file is above the %d-byte limit of the file tools; read part of it with grep or bash", maxFileBytes)

var imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

type content struct {
	data          []byte
	image         bool
	mediaType     string
	width, height int
}

var errNotRegular = errors.New("not a regular file")

// openRegular opens path for reading and fails with errNotRegular for an
// entry that is not a regular file after symlinks. The open does not wait for
// a writer of a named pipe, the type check is on the open file, and an entry
// that the system refuses to open, such as a socket, is classified by its
// type.
func openRegular(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|nonblock, 0)
	if err != nil {
		if info, serr := os.Stat(path); serr == nil && !info.Mode().IsRegular() && !info.IsDir() {
			return nil, errNotRegular
		}
		return nil, err
	}
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		if err == nil {
			err = errNotRegular
		}
		return nil, err
	}
	return f, nil
}

// readCapped reads path whole, or fails with errTooLarge or errNotRegular.
func readCapped(path string) ([]byte, error) {
	f, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFileBytes {
		return nil, &fs.PathError{Op: "read", Path: path, Err: errTooLarge}
	}
	return data, nil
}

// readContent reads path. The magic bytes, never the extension, classify
// it. A file that has the magic bytes of an image but does not decode as
// one is text.
func readContent(path string) (content, error) {
	data, err := readCapped(path)
	if err != nil {
		return content{}, err
	}
	if mediaType := http.DetectContentType(data); imageTypes[mediaType] {
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
			return content{data: data, image: true, mediaType: mediaType, width: cfg.Width, height: cfg.Height}, nil
		}
	}
	return content{data: data}, nil
}
