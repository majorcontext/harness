package engine

import (
	"bytes"
	"context"
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
// resolvePath maps a tool argument to the path the tool opens. Every file
// tool MUST route its path argument through it.
//
// That is load-bearing beyond convenience: filePathKey builds the batch
// executor's per-file exclusion key from resolvePath's own output (see
// filePathKey), so the key names the file a tool touches only while every
// tool resolves the same way. A new file tool that resolved a path by any
// other route would key one file under two names, and a concurrent write
// and edit on it would stop being serialized.
func (s *Session) resolvePath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(s.cfg.WorkDir, path)
}

// filePathKeyPrefix namespaces the resource key read_file, write_file, and
// edit_file share (see filePathKey). All three key on the same
// RESOLVED absolute path, so same-path calls across any of the three
// tools serialize together in call order, while different paths still run
// concurrently — a deliberate in-class widening of the approved
// "edit_file serializes per path" design: read_file and write_file join
// the same namespace because a read racing a concurrent write or edit to
// the SAME file is exactly the same same-file hazard class, just missing
// the "corruption" half (a stale read is still a wrong answer). See the
// Tool.Key field doc comment (engine.go) for the general contract.
const filePathKeyPrefix = "path:"

// filePathKey resolves a read_file/write_file/edit_file call's "path"
// argument against s's working directory (s.resolvePath), so a relative
// and an absolute argument naming the same file produce the same key. A
// call whose args do not parse, or carry no path, falls back to a fixed
// sentinel key rather than panicking — conservative because it still
// serializes every unparseable call against every other one, never
// against a well-formed call to an unrelated path.
func filePathKey(s *Session, args json.RawMessage) string {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.Path == "" {
		return filePathKeyPrefix + "<unparsed>"
	}
	// Make the resolved path ABSOLUTE, then clean it, so every spelling of
	// one file produces one key.
	//
	// Cleaning alone is not enough. resolvePath joins a relative argument
	// onto Config.WorkDir, and WorkDir itself may be relative — an
	// embedder sets that field directly. With WorkDir "." the argument
	// "a.txt" resolves to "a.txt" while "/cwd/a.txt" resolves to itself,
	// so one file would take two keys and a write could race an edit on
	// it. filepath.Abs resolves against the process working directory,
	// which is the same directory the tools' own os.Open/os.WriteFile
	// calls resolve against, so the key always names the file the tool
	// actually touches. Abs also cleans, which is what collapses the
	// "a/../x" alias.
	//
	// Abs fails only when the process working directory cannot be read.
	// Fall back to the cleaned path then: still correct whenever WorkDir
	// is absolute, which is what cmd/harness always passes.
	// resolvePath returns an absolute argument verbatim, and filepath.Join
	// cleans only the relative branch, so the key would otherwise miss a
	// dot-dot alias on an absolute path. The tools themselves open both
	// spellings through the same resolvePath, so cleaning here can only
	// merge two keys that name one file, never split one file into two.
	//
	// A symlink or a hard link that reaches one inode under two different
	// paths still keys as two resources. Resolving that needs a
	// filepath.EvalSymlinks (or a stat for inode identity) on every key,
	// which is a syscall on the batch's hot path and fails on a path that
	// does not exist yet — a write_file target routinely does not. This is
	// an accepted, documented residual, not an oversight.
	resolved := s.resolvePath(in.Path)
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return filePathKeyPrefix + filepath.Clean(resolved)
	}
	return filePathKeyPrefix + abs
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
		Key: filePathKey,
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
			Description: "Write content to a file, creating parent directories as needed and overwriting any existing file. Prefer this over shell redirection or heredocs for creating and rewriting files. Relative paths resolve against the session working directory.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "Path to the file to write"},
					"content": {"type": "string", "description": "Full content to write to the file"}
				},
				"required": ["path", "content"]
			}`),
		},
		Key: filePathKey,
		Run: func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
			var in struct {
				Path    string  `json:"path"`
				Content *string `json:"content"`
			}
			if err := json.Unmarshal(args, &in); err != nil || in.Path == "" || in.Content == nil {
				return nil, fmt.Errorf("write_file: missing path or content argument")
			}
			path := s.resolvePath(in.Path)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return nil, fmt.Errorf("write_file: %w", err)
			}
			if err := os.WriteFile(path, []byte(*in.Content), 0o644); err != nil {
				return nil, fmt.Errorf("write_file: %w", err)
			}
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
		Key: filePathKey,
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
			s.emitFileEdited(path)
			return message.Parts{&message.Text{Text: fmt.Sprintf("replaced %d occurrence(s) in %s", replaced, path)}}, nil
		},
	}
}
