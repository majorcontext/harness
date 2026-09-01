package server

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // register GIF decoder for image.DecodeConfig
	_ "image/jpeg" // register JPEG decoder for image.DecodeConfig
	_ "image/png"  // register PNG decoder for image.DecodeConfig
	"net/http"
	"strings"

	_ "golang.org/x/image/webp" // register WebP decoder for image.DecodeConfig

	"github.com/majorcontext/harness/message"
)

// Prompt attachments: the parsing and validation half of "a person can send
// a picture", shared by every handler that accepts a prompt body's `parts`
// array.
//
// The wire shape is message.Blob's own JSON, verbatim
// ({"type":"blob","media_type":...,"data":<base64>}), so a caller building a
// prompt uses the same vocabulary the transcript hands back to it — no
// second, prompt-only attachment format to keep in sync. What this file
// adds is admission control: harness accepts a blob only if the model can
// actually be shown it, and it says so synchronously, while the caller can
// still fix the upload.
//
// Why validation belongs HERE and not at transcode time: a user message is
// appended to an append-only durable log before any provider ever sees it
// (engine.Session.PromptWithOrigin). A blob no provider can accept would
// therefore be persisted first and rejected on every turn afterwards — the
// exact wedge imageclamp exists to heal (see imageclamp's package doc:
// three Neptune boxes, oversized screenshots, a 400 that survived respawn).
// imageclamp handles the sizes it can repair by downscaling; this gate
// handles the ones nothing can repair — a type no provider decodes, bytes
// that are not the image they claim to be, an attachment with no payload.

// promptBlobMediaTypes is the set of attachment media types a prompt may
// carry. It matches engine's own read_file image set
// (engine/filetools.go's readFileImageMediaTypes) deliberately: those are
// the formats every provider adapter transcodes and imageclamp can decode
// and downscale, so an uploaded image and a model-read image are the same
// class of thing everywhere downstream.
//
// Documents (application/pdf) are NOT in this set even though Anthropic
// accepts them: imageclamp cannot clamp a PDF, and provider support is
// uneven (see provider/openaicompat's blobURL restrictions), so accepting
// one here would be a promise this server cannot keep on every model a
// session can switch to.
var promptBlobMediaTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// promptBlobMaxBytes bounds ONE decoded attachment. It matches engine's
// read_file image ceiling (readFileMaxImageBytes) for the same reason the
// media-type set does: one limit for "an image harness will hold in a
// session", however it arrived.
//
// This is not the provider wire limit — imageclamp enforces those per
// adapter at transcode time, downscaling rather than rejecting. This cap
// exists so a single request cannot make the server hold an unbounded
// decoded payload, and so a caller sending something absurd learns
// immediately instead of after a resize it never asked for.
const promptBlobMaxBytes = 20 * 1024 * 1024

// promptParts is one decoded prompt body: the caller's text (every text
// part joined by newlines, exactly as the text-only contract always did)
// and its attachments in wire order.
type promptParts struct {
	Text  string
	Blobs []*message.Blob
}

// promptPartsInput is the JSON shape of one element of a prompt body's
// `parts` array — a text part or a blob part. Fields not valid for the
// part's own type are simply absent; decodePromptParts rejects a part whose
// type is neither.
type promptPartInput struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	MediaType string `json:"media_type"`
	Data      []byte `json:"data"`
	URL       string `json:"url"`
}

// errEmptyPromptParts reports a `parts` array that carried no deliverable
// content at all — no text, no attachment. Callers map it to the same 400
// an empty array already produced.
var errEmptyPromptParts = errors.New("parts must carry text or at least one attachment")

// decodePromptParts validates a prompt body's parts and folds them into the
// text-plus-attachments pair every prompt path downstream takes. It returns
// an HTTP status and error for a caller to write verbatim; a nil error
// means every part was usable.
//
// Order is preserved for attachments and irrelevant for text (joined, as
// before). Validation is total and happens BEFORE the caller claims a run
// slot or enqueues anything, so a rejected prompt leaves no trace: no
// message appended, no queue entry, no turn started.
func decodePromptParts(parts []promptPartInput) (promptParts, int, error) {
	var out promptParts
	var texts []string
	for _, p := range parts {
		switch p.Type {
		case "text":
			texts = append(texts, p.Text)
		case "blob":
			blob, err := decodePromptBlob(p)
			if err != nil {
				return promptParts{}, http.StatusBadRequest, err
			}
			out.Blobs = append(out.Blobs, blob)
		default:
			return promptParts{}, http.StatusBadRequest, fmt.Errorf("unsupported part type %q: text and blob parts only", p.Type)
		}
	}
	out.Text = strings.Join(texts, "\n")
	if strings.TrimSpace(out.Text) == "" && len(out.Blobs) == 0 {
		return promptParts{}, http.StatusBadRequest, errEmptyPromptParts
	}
	return out, 0, nil
}

// decodePromptBlob validates one blob part and returns the message.Blob it
// becomes. Every rejection names what is wrong with the attachment itself,
// never how to fix the request format, because the caller here is a UI
// forwarding a file a person chose.
func decodePromptBlob(p promptPartInput) (*message.Blob, error) {
	if !promptBlobMediaTypes[p.MediaType] {
		return nil, fmt.Errorf("unsupported blob media type %q: prompt attachments must be image/png, image/jpeg, image/gif, or image/webp", p.MediaType)
	}
	if len(p.Data) == 0 {
		if p.URL != "" {
			// A URL blob is a valid message.Blob and some providers accept
			// one, but harness would then be asking every provider (and
			// imageclamp, which must decode bytes to clamp them) to fetch
			// caller-supplied URLs from inside the box. That is an
			// egress decision this route does not get to make on its own.
			return nil, errors.New("a prompt attachment must carry inline data, not a url")
		}
		return nil, errors.New("blob has neither data nor url")
	}
	if len(p.Data) > promptBlobMaxBytes {
		return nil, fmt.Errorf("attachment is %d bytes, which exceeds the %d-byte limit for one prompt attachment", len(p.Data), promptBlobMaxBytes)
	}
	// Decode the header, not just the magic bytes: a truncated or corrupt
	// image passes a prefix check and then fails at the provider, on every
	// later turn, from inside a durable transcript. DecodeConfig reads the
	// dimensions only, so this costs a header parse rather than a full
	// decode of a 20MB image.
	cfg, format, err := image.DecodeConfig(bytes.NewReader(p.Data))
	if err != nil {
		return nil, fmt.Errorf("attachment does not decode as %s: %v", p.MediaType, err)
	}
	if !formatMatchesMediaType(format, p.MediaType) {
		return nil, fmt.Errorf("attachment does not decode as %s: the data is %s", p.MediaType, format)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, fmt.Errorf("attachment does not decode as %s: it reports a %dx%d size", p.MediaType, cfg.Width, cfg.Height)
	}
	return &message.Blob{MediaType: p.MediaType, Data: p.Data}, nil
}

// formatMatchesMediaType reports whether image.DecodeConfig's format name
// (the registered decoder's own name: "png", "jpeg", "gif", "webp") is the
// one the caller's media type claims. A mismatch is not pedantry: a JPEG
// labeled image/png reaches the provider as a lie about its own bytes, and
// the provider — not harness — is the one that rejects it, one turn later
// and from inside a durable transcript.
func formatMatchesMediaType(format, mediaType string) bool {
	return "image/"+format == mediaType
}
