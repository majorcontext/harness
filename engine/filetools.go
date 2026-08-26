package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"  // register GIF decoder for image.DecodeConfig
	_ "image/jpeg" // register JPEG decoder for image.DecodeConfig
	_ "image/png"  // register PNG decoder for image.DecodeConfig
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	_ "golang.org/x/image/webp" // register WebP decoder for image.DecodeConfig

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

const (
	// readFileDefaultLimit is the default maximum number of lines returned
	// by read_file.
	readFileDefaultLimit = 2000
	// readFileMaxLineLen is the maximum length of a single returned line;
	// longer lines are truncated with a trailing ellipsis.
	readFileMaxLineLen = 2000
)

// readFileMaxImageBytes bounds the total bytes read_file will read from a
// detected image. The transcode-time imageclamp pass (imageclamp.Clamp)
// enforces each provider's own wire size limit; this cap exists only to
// stop read_file itself from loading an unbounded file into memory before
// that clamp ever runs. readPathContent checks this bound against the
// read itself (an io.LimitReader over the open file handle), never against
// a separately captured os.Stat size, which a file that grows after the
// stat could outrun. It is a var, not a const, so a test can shrink it
// instead of writing a real oversized fixture.
var readFileMaxImageBytes = 20 * 1024 * 1024 // 20MB

// imageSniffLen is how many leading bytes classify a file by magic bytes —
// the same bound http.DetectContentType itself considers.
const imageSniffLen = 512

// readFileImageMediaTypes lists the image formats read_file recognizes and
// returns as a message.Blob for the model to see directly. Every other
// sniffed content type — including any other image/* MIME type
// http.DetectContentType might report, such as image/bmp — keeps
// read_file's existing text-reading behavior unchanged.
var readFileImageMediaTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// sniffMediaType reads up to imageSniffLen bytes from r via io.ReadFull and
// classifies them via http.DetectContentType, returning the classification
// and the sniffed bytes themselves (so a caller reading the rest of the
// stream does not re-read them). Taking an io.Reader, not a path, is what
// lets TestSniffMediaTypeSurvivesShortReads drive it with
// iotest.OneByteReader: a plain single Read against such a source returns
// one byte per call, so a caller using Read directly would misclassify
// almost every real image — io.ReadFull is what makes this deterministic
// against a short-read source (a pipe, a FUSE/network mount, a signal).
func sniffMediaType(r io.Reader) (mediaType string, sniffed []byte, err error) {
	buf := make([]byte, imageSniffLen)
	n, rerr := io.ReadFull(r, buf)
	if rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF {
		return "", nil, rerr
	}
	buf = buf[:n]
	return http.DetectContentType(buf), buf, nil
}

// fileContent is the outcome of readPathContent: either a detected image's
// Blob-ready bytes, or a non-image file's raw bytes for the ordinary text
// read. Exactly one of ImageData or TextData is populated.
type fileContent struct {
	IsImage       bool
	ImageData     []byte
	MediaType     string
	Width, Height int
	TextData      []byte
}

// readPathContent opens path exactly once and, on every outcome, reads it
// exactly once — no second os.Open, no re-read of bytes already
// classified. Earlier revisions of read_file's image detection opened the
// file twice (once to sniff, once via os.ReadFile) even on a plain text
// read, the far more common case; this unifies both outcomes behind one
// handle.
//
// Classification is by magic bytes only (http.DetectContentType via
// sniffMediaType), never by the file's extension: a ".txt" that is really
// a PNG is still recognized; a ".png" that is really text is not.
//
// A recognized image type is read under a bound: readFileMaxImageBytes+1
// bytes via io.LimitReader over the SAME open handle the sniff used — a
// cap that binds on bytes actually read, never a pre-read os.Stat size a
// concurrently growing file could outrun. An over-cap image returns a
// non-nil error (no partial TextData) so the caller reports a clear error
// instead of falling through to an unbounded read of a file already known
// to be an oversized image.
//
// The image body must also decode with image.DecodeConfig before this
// function commits to the image outcome: a corrupt or truncated file that
// merely opens with a matching magic-byte prefix (PNG's 8-byte signature,
// JPEG's SOI marker, WebP's RIFF/WEBP container) fails PNG/JPEG/WebP's own
// structural header checks. On that failure this reads the true remainder
// of the file (unbounded, via the same handle) so the text fallback is
// complete rather than silently cut off at the image cap — reachable only
// when magic bytes matched an image signature yet the body failed
// structural validation, so an oversized file in this branch is already
// known not to be a real image. This gate is NOT airtight for GIF: the
// GIF87a/GIF89a header has no checksum, so any bytes following a literal
// "GIF87a"/"GIF89a" prefix still "decode" with fabricated width/height. A
// real-world text file beginning with those exact six bytes is
// vanishingly unlikely; this is a documented, accepted residual, not a
// silent gap.
func readPathContent(path string) (fileContent, error) {
	f, err := os.Open(path)
	if err != nil {
		return fileContent{}, err
	}
	defer f.Close()

	mediaType, sniff, err := sniffMediaType(f)
	if err != nil {
		return fileContent{}, err
	}
	if !readFileImageMediaTypes[mediaType] {
		rest, err := io.ReadAll(f)
		if err != nil {
			return fileContent{}, err
		}
		return fileContent{TextData: append(sniff, rest...)}, nil
	}

	budget := int64(readFileMaxImageBytes) - int64(len(sniff))
	if budget < 0 {
		budget = 0
	}
	capped, err := io.ReadAll(io.LimitReader(f, budget+1))
	if err != nil {
		return fileContent{}, err
	}
	full := append(sniff, capped...)
	if len(full) > readFileMaxImageBytes {
		return fileContent{}, fmt.Errorf("image (%s) exceeds the %d-byte read_file image limit", mediaType, readFileMaxImageBytes)
	}

	cfg, _, derr := image.DecodeConfig(bytes.NewReader(full))
	if derr != nil {
		// Sniffed as an image by magic bytes, but the body does not decode
		// as one (corrupt, truncated, or a false-positive magic-byte
		// match). Read the true remainder so the text fallback is
		// complete, not silently cut off at the image cap; see the doc
		// comment above for why this is safe.
		rest, err := io.ReadAll(f)
		if err != nil {
			return fileContent{}, err
		}
		return fileContent{TextData: append(full, rest...)}, nil
	}
	return fileContent{IsImage: true, ImageData: full, MediaType: mediaType, Width: cfg.Width, Height: cfg.Height}, nil
}

// resolvePath resolves a tool path argument against the session working
// directory. Absolute paths pass through unchanged.
func (s *Session) resolvePath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(s.cfg.WorkDir, path)
}

// recordRead records that this session has seen resolvedPath's raw on-disk
// bytes hash to hash, either because read_file just read them or because
// write_file/edit_file just wrote them. resolvedPath must already be an
// s.resolvePath output — every caller in this file passes one, so the map
// never keys on a raw, unresolved tool argument. See the readHashes field
// doc comment (engine.go) and the "write_file read-before-overwrite guard"
// section of AGENTS.md for the full design.
func (s *Session) recordRead(resolvedPath string, hash [sha256.Size]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readHashes[resolvedPath] = hash
}

// readHashFor reports the hash last recorded for resolvedPath and whether
// this session has ever read or written it at all.
func (s *Session) readHashFor(resolvedPath string) (hash [sha256.Size]byte, everRead bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hash, everRead = s.readHashes[resolvedPath]
	return hash, everRead
}

// hashFileContent returns the sha256 hash of path's complete current bytes,
// read fresh from disk. write_file uses it to detect whether an
// already-read file changed on disk since this session last saw it (a
// concurrent writer, an external process, a `bash` command) — the recorded
// hash alone is not enough, since a stale record would let a write through
// against content the session's last read no longer describes.
func hashFileContent(path string) ([sha256.Size]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(data), nil
}

func readFileTool() Tool {
	return Tool{
		Def: provider.ToolDef{
			Name:        "read_file",
			Description: "Read a file and return its content with line numbers (N→ prefixes). A recognized image file (PNG, JPEG, GIF, WebP) is returned as an image where the current provider supports tool-result images. Prefer this over shell commands like cat, head, or sed for reading files. Relative paths resolve against the session working directory.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "Path to the file to read"},
					"offset": {"type": "integer", "description": "1-based line number to start reading from"},
					"limit": {"type": "integer", "description": "Maximum number of lines to return (default 2000)"}
				},
				"required": ["path"]
			}`),
		},
		Run: func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
			var in struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
			}
			if err := json.Unmarshal(args, &in); err != nil || in.Path == "" {
				return nil, fmt.Errorf("read_file: missing path argument")
			}
			path := s.resolvePath(in.Path)
			info, err := os.Stat(path)
			if err != nil {
				return nil, fmt.Errorf("read_file: %w", err)
			}
			if info.IsDir() {
				return nil, fmt.Errorf("read_file: %s is a directory", path)
			}

			content, err := readPathContent(path)
			if err != nil {
				return nil, fmt.Errorf("read_file: %s: %w", path, err)
			}
			rawBytes := content.TextData
			if content.IsImage {
				rawBytes = content.ImageData
			}
			// Record the read-before-overwrite guard's hash from the RAW
			// bytes readPathContent already read off disk, never the
			// offset/limit-sliced text below — read_file always reads the
			// whole file regardless of what window it goes on to display.
			// This runs for every successful read, including one whose
			// requested offset later turns out to be past end-of-file: the
			// disk read itself still happened, before the model made any
			// request the guard needs to have honored.
			s.recordRead(path, sha256.Sum256(rawBytes))
			if content.IsImage {
				summary := fmt.Sprintf("image (%s), %d bytes, %dx%d pixels", content.MediaType, len(content.ImageData), content.Width, content.Height)
				return message.Parts{
					&message.Text{Text: summary},
					&message.Blob{MediaType: content.MediaType, Data: content.ImageData},
				}, nil
			}
			data := content.TextData

			lines := strings.Split(string(data), "\n")
			// A trailing newline produces one empty trailing element; drop it.
			if n := len(lines); n > 0 && lines[n-1] == "" {
				lines = lines[:n-1]
			}
			total := len(lines)

			offset := in.Offset
			if offset < 1 {
				offset = 1
			}
			limit := in.Limit
			if limit <= 0 {
				limit = readFileDefaultLimit
			}
			if total == 0 {
				return message.Parts{&message.Text{Text: "(empty file)"}}, nil
			}
			if offset > total {
				return nil, fmt.Errorf("read_file: offset %d is past end of file (%d lines)", offset, total)
			}
			end := offset + limit - 1
			if end > total {
				end = total
			}

			var b strings.Builder
			for i := offset; i <= end; i++ {
				line := lines[i-1]
				if r := []rune(line); len(r) > readFileMaxLineLen {
					line = string(r[:readFileMaxLineLen]) + "…"
				}
				fmt.Fprintf(&b, "%d→%s\n", i, line)
			}
			out := strings.TrimSuffix(b.String(), "\n")
			if end < total {
				out += fmt.Sprintf("\n[truncated: showing lines %d-%d of %d]", offset, end, total)
			}
			return message.Parts{&message.Text{Text: out}}, nil
		},
	}
}

func writeFileTool() Tool {
	return Tool{
		Def: provider.ToolDef{
			Name:        "write_file",
			Description: "Write content to a file, creating parent directories as needed. Overwriting an existing file requires having read it first with read_file this session, with no changes on disk since — use edit_file for a targeted change, or read_file then write_file to intentionally replace it. Relative paths resolve against the session working directory.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "Path to the file to write"},
					"content": {"type": "string", "description": "Full content to write to the file"}
				},
				"required": ["path", "content"]
			}`),
		},
		Run: func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
			var in struct {
				Path    string  `json:"path"`
				Content *string `json:"content"`
			}
			if err := json.Unmarshal(args, &in); err != nil || in.Path == "" || in.Content == nil {
				return nil, fmt.Errorf("write_file: missing path or content argument")
			}
			path := s.resolvePath(in.Path)

			// Read-before-overwrite guard: only an EXISTING regular file is
			// gated — creation is write_file's main job, so a path os.Stat
			// cannot resolve to an existing regular file (missing, or any
			// stat error) falls straight through unguarded, exactly as
			// before this guard existed. See AGENTS.md's "write_file
			// read-before-overwrite guard" section for the full design.
			if info, statErr := os.Stat(path); statErr == nil && !info.IsDir() {
				recorded, everRead := s.readHashFor(path)
				if !everRead {
					return nil, fmt.Errorf("write_file: %s exists and has not been read this session; read it first (or use edit_file)", path)
				}
				current, err := hashFileContent(path)
				if err != nil {
					return nil, fmt.Errorf("write_file: %w", err)
				}
				if current != recorded {
					return nil, fmt.Errorf("write_file: %s changed on disk since it was read; read it again before overwriting", path)
				}
			}

			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return nil, fmt.Errorf("write_file: %w", err)
			}
			if err := os.WriteFile(path, []byte(*in.Content), 0o644); err != nil {
				return nil, fmt.Errorf("write_file: %w", err)
			}
			// The bytes just written are now, by definition, what this
			// session has "seen" at this path — record/update the hash so
			// an immediate follow-up write to the SAME path (this session
			// overwriting its own just-written content) never spuriously
			// re-triggers the guard above.
			s.recordRead(path, sha256.Sum256([]byte(*in.Content)))
			s.emitFileEdited(path)
			return message.Parts{&message.Text{Text: fmt.Sprintf("wrote %d bytes to %s", len(*in.Content), path)}}, nil
		},
	}
}

func editFileTool() Tool {
	return Tool{
		Def: provider.ToolDef{
			Name:        "edit_file",
			Description: "Replace an exact string in a file. Prefer this over sed or shell heredocs for editing files. old_string must match the file content exactly and uniquely; include surrounding context to disambiguate, or set replace_all to replace every occurrence. Relative paths resolve against the session working directory.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "Path to the file to edit"},
					"old_string": {"type": "string", "description": "Exact text to replace"},
					"new_string": {"type": "string", "description": "Replacement text"},
					"replace_all": {"type": "boolean", "description": "Replace every occurrence (default false)"}
				},
				"required": ["path", "old_string", "new_string"]
			}`),
		},
		Run: func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
			var in struct {
				Path       string `json:"path"`
				OldString  string `json:"old_string"`
				NewString  string `json:"new_string"`
				ReplaceAll bool   `json:"replace_all"`
			}
			if err := json.Unmarshal(args, &in); err != nil || in.Path == "" || in.OldString == "" {
				return nil, fmt.Errorf("edit_file: missing path or old_string argument")
			}
			if in.OldString == in.NewString {
				return nil, fmt.Errorf("edit_file: old_string and new_string are identical")
			}
			path := s.resolvePath(in.Path)
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("edit_file: %w", err)
			}
			content := string(data)

			count := strings.Count(content, in.OldString)
			switch {
			case count == 0:
				return nil, fmt.Errorf("edit_file: old_string not found in %s", path)
			case count > 1 && !in.ReplaceAll:
				return nil, fmt.Errorf("edit_file: old_string matches %d times in %s; provide more surrounding context to make it unique, or set replace_all to true", count, path)
			}

			replaced := count
			if !in.ReplaceAll {
				replaced = 1
			}
			content = strings.Replace(content, in.OldString, in.NewString, replaced)
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return nil, fmt.Errorf("edit_file: %w", err)
			}
			// Update the read-before-overwrite guard's hash to the new
			// content: edit_file's own exact-match requirement already
			// proves the model saw the pre-edit content, and the file now
			// holds exactly what this session just wrote — a later
			// write_file to this same path must not have to read_file
			// again to learn what edit_file already put there.
			s.recordRead(path, sha256.Sum256([]byte(content)))
			s.emitFileEdited(path)
			return message.Parts{&message.Text{Text: fmt.Sprintf("replaced %d occurrence(s) in %s", replaced, path)}}, nil
		},
	}
}
