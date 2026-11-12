package turn

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/majorcontext/harness/protocol"
)

// AttachmentDir is the directory under WorkDir that holds file attachments.
const AttachmentDir = ".harness/attachments"

// AttachmentNote puts the file p at <workDir>/AttachmentDir/<sha256>/<name>, reading its blob only when that file is missing or the wrong size, and returns its tag.
func AttachmentNote(workDir string, p protocol.LogPart, read func(key string) ([]byte, error)) (string, error) {
	if workDir == "" {
		return "", fmt.Errorf("attachment %s: the session has no work directory to hold it", p.BlobKey)
	}
	dir, err := filepath.Abs(filepath.Join(workDir, AttachmentDir, safeSegment(strings.TrimPrefix(p.BlobKey, "attachment-"))))
	if err != nil {
		return "", fmt.Errorf("attachment %s: %w", p.BlobKey, err)
	}
	path := filepath.Join(dir, safeSegment(p.Name))
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() || st.Size() != int64(p.Bytes) {
		data, err := read(p.BlobKey)
		if err != nil {
			return "", fmt.Errorf("attachment %s: %w", p.BlobKey, err)
		}
		if err := writeFile(dir, path, data); err != nil {
			return "", fmt.Errorf("attachment %s: write %s: %w", p.BlobKey, path, err)
		}
	}
	return `<harness-attachment name="` + escapeAttr(p.Name) + `" path="` + escapeAttr(path) + `" media_type="` + escapeAttr(p.MediaType) +
		`" bytes="` + strconv.Itoa(p.Bytes) + `"/>`, nil
}

func safeSegment(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if name = strings.ReplaceAll(name, "..", ""); name == "" || name == "." {
		return "attachment"
	}
	return name
}

func writeFile(dir, path string, data []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".attachment-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	return nil
}

func escapeAttr(v string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(v))
	return b.String()
}
