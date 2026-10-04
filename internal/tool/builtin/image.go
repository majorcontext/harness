package builtin

import (
	"bytes"
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

// readCapped reads path whole, or fails with errTooLarge.
func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
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
