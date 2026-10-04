// Package admit turns a protocol input into the record that the log holds.
// It checks each part before anything is stored, so a rejected input leaves
// no trace: an attachment that no provider can take would otherwise stay in
// the history and fail every later turn.
package admit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/gif"  // register the GIF decoder for image.DecodeConfig
	_ "image/jpeg" // register the JPEG decoder for image.DecodeConfig
	_ "image/png"  // register the PNG decoder for image.DecodeConfig
	"slices"
	"strings"

	_ "golang.org/x/image/webp" // register the WebP decoder for image.DecodeConfig

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/protocol"
)

// MaxAttachmentBytes bounds one attachment. A PDF past the request ceiling
// of a provider would fail every turn, and nothing can repair a document.
const MaxAttachmentBytes = 20 << 20

// Blob is the bytes of an attachment, to store under Key before the record
// that names it.
type Blob struct {
	Key  string
	Data []byte
}

// attachmentTypes are the media types that every provider takes or degrades,
// each with the check that proves the bytes are that type.
var attachmentTypes = map[string]func(mediaType string, data []byte) error{
	"image/png":       verifyImage,
	"image/jpeg":      verifyImage,
	"image/gif":       verifyImage,
	"image/webp":      verifyImage,
	"application/pdf": verifyPDF,
}

// Input returns the record of in and the blobs of its attachments. Its error
// names the part that is not valid.
func Input(in protocol.Input) (eventlog.InputAdmitted, []Blob, error) {
	ev := eventlog.InputAdmitted{InputID: in.ID, Delivery: eventlog.Delivery(in.Delivery), Source: in.Source}
	if ev.Delivery == "" {
		ev.Delivery = eventlog.DeliverySteer
	}
	if ev.Source == "" {
		ev.Source = "user"
	}
	switch {
	case in.ID == "":
		return ev, nil, fmt.Errorf("input id is empty")
	case len(in.Parts) == 0:
		return ev, nil, fmt.Errorf("input %s has no parts", in.ID)
	case ev.Delivery != eventlog.DeliveryQueue && ev.Delivery != eventlog.DeliverySteer:
		return ev, nil, fmt.Errorf("input %s has delivery %q", in.ID, in.Delivery)
	}
	var blobs []Blob
	for _, p := range in.Parts {
		switch p.Type {
		case protocol.PartText:
			ev.Parts = append(ev.Parts, eventlog.Part{Type: eventlog.PartText, Text: p.Text})
		case protocol.PartBlob:
			part, err := attachment(p)
			if err != nil {
				return ev, nil, fmt.Errorf("input %s: %w", in.ID, err)
			}
			ev.Parts = append(ev.Parts, part)
			blobs = append(blobs, Blob{Key: part.BlobKey, Data: p.Data})
		default:
			return ev, nil, fmt.Errorf("input %s has part type %q", in.ID, p.Type)
		}
	}
	return ev, blobs, nil
}

// attachment checks one blob part and returns the part that the log holds.
// The key is the hash of the bytes, so one attachment sent twice is one blob.
func attachment(p protocol.Part) (eventlog.Part, error) {
	verify, ok := attachmentTypes[p.MediaType]
	switch {
	case !ok:
		types := make([]string, 0, len(attachmentTypes))
		for t := range attachmentTypes {
			types = append(types, t)
		}
		slices.Sort(types)
		return eventlog.Part{}, fmt.Errorf("unsupported attachment media type %q: use one of %s", p.MediaType, strings.Join(types, ", "))
	case len(p.Data) == 0:
		return eventlog.Part{}, fmt.Errorf("attachment %s has no data", p.MediaType)
	case len(p.Data) > MaxAttachmentBytes:
		return eventlog.Part{}, fmt.Errorf("attachment is %d bytes, which exceeds the %d-byte limit", len(p.Data), MaxAttachmentBytes)
	}
	if err := verify(p.MediaType, p.Data); err != nil {
		return eventlog.Part{}, fmt.Errorf("attachment %w", err)
	}
	sum := sha256.Sum256(p.Data)
	return eventlog.Part{Type: eventlog.PartBlob, MediaType: p.MediaType, BlobKey: "attachment-" + hex.EncodeToString(sum[:]), Bytes: len(p.Data)}, nil
}

// verifyImage proves that data decodes as the image type that it claims. It
// reads the header only.
func verifyImage(mediaType string, data []byte) error {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("does not decode as %s: %v", mediaType, err)
	}
	if decoded := "image/" + format; decoded != mediaType {
		return fmt.Errorf("does not decode as %s: the data is %s", mediaType, decoded)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return fmt.Errorf("does not decode as %s: it reports a %dx%d size", mediaType, cfg.Width, cfg.Height)
	}
	return nil
}

// verifyPDF proves that data begins as a PDF does. A broken PDF is the
// business of the provider; a JPEG that claims to be a PDF is ours.
func verifyPDF(mediaType string, data []byte) error {
	if !bytes.HasPrefix(data, []byte("%PDF-")) {
		return fmt.Errorf("does not begin with a %%PDF- header, so it is not a %s", mediaType)
	}
	return nil
}
