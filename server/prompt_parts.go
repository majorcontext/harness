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
	"sort"
	"strings"

	_ "golang.org/x/image/webp" // register WebP decoder for image.DecodeConfig

	"github.com/majorcontext/harness/message"
)

// Prompt attachments: the parsing and validation half of "a person can send
// a file" -- an image or a PDF, the set promptAttachmentTypes admits below --
// shared by every handler that accepts a prompt body's `parts` array.
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

// promptAttachmentTypes is the set of attachment media types a prompt may
// carry, each paired with the check that proves the bytes really are that
// type. A media type belongs here only when EVERY provider lane harness can
// dispatch to actually delivers it, because a session switches models
// freely and the attachment stays in its history forever:
//
//   - Images (the same set engine's read_file returns as a blob, see
//     readFileImageMediaTypes): anthropic and claude-code send an image
//     block, openai an input_image, openaicompat a data URL. imageclamp can
//     also decode and downscale them, so an oversized one heals rather than
//     wedging a session.
//   - application/pdf: anthropic and claude-code send a document block,
//     openai an input_file. Verified against the real claude-code CLI, which
//     read a PDF's text back through its stream-json stdin.
//
// Everything else stays out for a concrete reason, not caution: a
// text/plain or docx blob reaches openai's transcodeBlob as "unsupported
// blob media type" and openaicompat's blobURL the same way, so accepting
// one here would be a promise two lanes cannot keep. Widening this set is a
// row plus a verifier — and a check that every provider adapter transcodes
// the new type.
var promptAttachmentTypes = map[string]func(mediaType string, data []byte) error{
	"image/png":       verifyImageBytes,
	"image/jpeg":      verifyImageBytes,
	"image/gif":       verifyImageBytes,
	"image/webp":      verifyImageBytes,
	"application/pdf": verifyPDFBytes,
}

// promptAttachmentMaxBytes bounds ONE decoded attachment. For an image it
// matches engine's read_file ceiling (readFileMaxImageBytes): one limit for
// "a picture harness will hold in a session", however it arrived.
//
// For a PDF the cap does more work, and is the only protection there is:
// imageclamp decodes and downscales an oversized IMAGE at transcode time,
// but it cannot rewrite a document, so a PDF past a provider's own request
// ceiling (Anthropic's is 32MB) would fail every turn from inside a durable
// transcript with nothing able to repair it. This ceiling sits below that.
const promptAttachmentMaxBytes = 20 * 1024 * 1024

// promptRequestMaxBytes bounds the WHOLE prompt request body, before any of
// it is decoded.
//
// promptAttachmentMaxBytes above is checked per attachment, but only AFTER
// encoding/json has already base64-decoded that attachment into a []byte --
// and nothing bounds how many attachments one body may carry. So without a
// bound here, a single request could make this server allocate an unbounded
// amount before the first size check ever ran, and the check would then
// reject what it had already paid for.
//
// 32 MiB is the same ceiling Anthropic applies to a whole request, which is
// the real limit a prompt has to fit inside anyway. Base64 costs about 4/3,
// so this admits one attachment at the full 20 MiB per-attachment cap
// (~26.7 MiB encoded) plus its text, or several smaller ones -- while a body
// that could never be delivered to a provider is refused here, cheaply,
// instead of after the allocation.
const promptRequestMaxBytes = 32 * 1024 * 1024

// verifyImageBytes proves data decodes as the image type it claims. It
// reads the header only (dimensions, not pixels), so a 20MB image costs a
// header parse rather than a full decode.
func verifyImageBytes(mediaType string, data []byte) error {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("does not decode as %s: %v", mediaType, err)
	}
	if decoded := "image/" + format; decoded != mediaType {
		// Report the DECODED MEDIA TYPE, not image.DecodeConfig's bare
		// format name ("jpeg"): the comparison just above is between two
		// media types, so naming one of them in the other's units leaves
		// the caller to guess whether "jpeg" meant image/jpeg. Both sides
		// of a mismatch now read in the same vocabulary the caller sent.
		return fmt.Errorf("does not decode as %s: the data is %s", mediaType, decoded)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return fmt.Errorf("does not decode as %s: it reports a %dx%d size", mediaType, cfg.Width, cfg.Height)
	}
	return nil
}

// verifyPDFBytes proves data is a PDF by its header. A PDF file begins with
// %PDF- followed by its version (ISO 32000-1 §7.5.2); this is a
// mislabeling check, not a validity check — a structurally broken PDF is
// the provider's business, while a JPEG labeled application/pdf is this
// server's.
func verifyPDFBytes(mediaType string, data []byte) error {
	if !bytes.HasPrefix(data, []byte("%PDF-")) {
		return fmt.Errorf("does not begin with a %%PDF- header, so it is not a %s", mediaType)
	}
	return nil
}

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
	verify, ok := promptAttachmentTypes[p.MediaType]
	if !ok {
		return nil, fmt.Errorf("unsupported blob media type %q: prompt attachments must be one of %s", p.MediaType, promptAttachmentTypeList())
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
	if len(p.Data) > promptAttachmentMaxBytes {
		return nil, fmt.Errorf("attachment is %d bytes, which exceeds the %d-byte limit for one prompt attachment", len(p.Data), promptAttachmentMaxBytes)
	}
	// Prove the bytes are the type they claim BEFORE the prompt is accepted:
	// a mislabeled or truncated file passes a media-type string check and
	// then fails at the provider, on every later turn, from inside a durable
	// transcript. Each type brings its own verifier (promptAttachmentTypes).
	if err := verify(p.MediaType, p.Data); err != nil {
		return nil, fmt.Errorf("attachment %v", err)
	}
	return &message.Blob{MediaType: p.MediaType, Data: p.Data}, nil
}

// promptAttachmentTypeList renders the accepted media types for an error
// message, sorted so the same request always names them in the same order.
func promptAttachmentTypeList() string {
	types := make([]string, 0, len(promptAttachmentTypes))
	for mediaType := range promptAttachmentTypes {
		types = append(types, mediaType)
	}
	sort.Strings(types)
	return strings.Join(types, ", ")
}
