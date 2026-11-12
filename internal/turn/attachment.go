package turn

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/majorcontext/harness/internal/admit"
	"github.com/majorcontext/harness/protocol"
)

// AttachmentDir is the directory under WorkDir that holds the attachments of the session.
const AttachmentDir = ".harness/attachments"

// AttachmentNote writes the bytes of the file attachment parts[i], which no
// provider takes, to AttachmentDir under workDir and returns the self-closing
// tag that tells the model where the file is. read returns the bytes of a
// blob key; it is called only when the file is missing or has another size.
// A write that fails fails the turn, so the model is never told of a file
// that is not there.
func AttachmentNote(workDir string, parts []protocol.LogPart, i int, read func(key string) ([]byte, error)) (string, error) {
	p := parts[i]
	if workDir == "" {
		return "", fmt.Errorf("attachment %s: the session has no work directory to hold it", p.BlobKey)
	}
	dir, err := filepath.Abs(filepath.Join(workDir, AttachmentDir, safeSegment(p.InputID)))
	if err != nil {
		return "", fmt.Errorf("attachment %s: %w", p.BlobKey, err)
	}
	path := filepath.Join(dir, uniqueName(parts, i))
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

// uniqueName returns the file name of parts[i]. A later file of the same input
// whose safe name is taken gets -2, -3, and so on before the extension, in part
// order, so one history always gives the same names.
func uniqueName(parts []protocol.LogPart, i int) string {
	taken := map[string]bool{}
	for j := 0; ; j++ {
		p := parts[j]
		if p.Type != protocol.LogPartBlob || admit.IsNative(p.MediaType) || p.InputID != parts[i].InputID {
			continue
		}
		name := safeSegment(p.Name)
		ext := filepath.Ext(name)
		stem := strings.TrimSuffix(name, ext)
		for n := 2; taken[name]; n++ {
			name = stem + "-" + strconv.Itoa(n) + ext
		}
		taken[name] = true
		if j == i {
			return name
		}
	}
}

// safeSegment returns name without a path separator, "..", NUL, or control
// character, or "attachment" when nothing is left.
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
