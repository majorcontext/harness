package admit_test

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/internal/admit"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/protocol"
)

// png is a 1x1 PNG.
func png() []byte {
	chunk := func(kind string, data []byte) []byte {
		var b bytes.Buffer
		_ = binary.Write(&b, binary.BigEndian, uint32(len(data)))
		b.WriteString(kind)
		b.Write(data)
		_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(kind), data...)))
		return b.Bytes()
	}
	ihdr := []byte{0, 0, 0, 1, 0, 0, 0, 1, 8, 0, 0, 0, 0}
	idat := []byte{0x78, 0x9c, 0x63, 0x60, 0x00, 0x00, 0x00, 0x02, 0x00, 0x01}
	return slices.Concat([]byte("\x89PNG\r\n\x1a\n"), chunk("IHDR", ihdr), chunk("IDAT", idat), chunk("IEND", nil))
}

func text(s string) protocol.Part { return protocol.Part{Type: protocol.PartText, Text: s} }

func blob(mediaType string, data []byte) protocol.Part {
	return protocol.Part{Type: protocol.PartBlob, MediaType: mediaType, Data: data}
}

func TestInputChecksTheEnvelopeAndEachPart(t *testing.T) {
	big := append([]byte("%PDF-"), make([]byte, admit.MaxAttachmentBytes)...)
	for _, tc := range []struct {
		name  string
		in    protocol.Input
		wants string
	}{
		{"an empty ID", protocol.Input{Parts: []protocol.Part{text("x")}}, "input id is empty"},
		{"no parts", protocol.Input{ID: "a"}, "has no parts"},
		{"an unknown delivery", protocol.Input{ID: "a", Delivery: "later", Parts: []protocol.Part{text("x")}}, `has delivery "later"`},
		{"an unknown part type", protocol.Input{ID: "a", Parts: []protocol.Part{{Type: "video"}}}, `has part type "video"`},
		{"a media type that no provider takes", protocol.Input{ID: "a", Parts: []protocol.Part{blob("text/plain", []byte("x"))}}, "unsupported attachment media type"},
		{"a blob with no data", protocol.Input{ID: "a", Parts: []protocol.Part{blob("image/png", nil)}}, "has no data"},
		{"bytes that are not the image they claim", protocol.Input{ID: "a", Parts: []protocol.Part{blob("image/jpeg", png())}}, "does not decode as image/jpeg"},
		{"a PDF with no header", protocol.Input{ID: "a", Parts: []protocol.Part{blob("application/pdf", []byte("nope"))}}, "does not begin with a %PDF- header"},
		{"an attachment over the limit", protocol.Input{ID: "a", Parts: []protocol.Part{blob("application/pdf", big)}}, "exceeds the"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := admit.Input(tc.in); err == nil || !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("Input = %v, want an error that holds %q", err, tc.wants)
			}
		})
	}
}

func TestInputKeepsTextAndStoresEachAttachmentUnderItsHash(t *testing.T) {
	data := png()
	ev, blobs, err := admit.Input(protocol.Input{ID: "a", Source: "user", Parts: []protocol.Part{text("look"), blob("image/png", data), blob("application/pdf", []byte("%PDF-1.7"))}})
	if err != nil {
		t.Fatal(err)
	}
	if ev.InputID != "a" || ev.Delivery != eventlog.DeliveryQueue || ev.Source != "user" || len(ev.Parts) != 3 {
		t.Fatalf("record = %+v, want input a, queued, from the user, with 3 parts", ev)
	}
	img := ev.Parts[1]
	if img.Type != eventlog.PartBlob || img.MediaType != "image/png" || img.Bytes != len(data) {
		t.Errorf("image part = %+v", img)
	}
	if len(blobs) != 2 || blobs[0].Key != img.BlobKey || !bytes.Equal(blobs[0].Data, data) || blobs[0].Key == blobs[1].Key {
		t.Errorf("blobs = %+v, want the image under its part key, and a second key for the PDF", blobs)
	}
	if _, again, _ := admit.Input(protocol.Input{ID: "b", Parts: []protocol.Part{blob("image/png", data)}}); again[0].Key != img.BlobKey {
		t.Errorf("the same bytes got key %s, want %s", again[0].Key, img.BlobKey)
	}
}

func TestInputTakesAnAttachmentWithNoText(t *testing.T) {
	if _, _, err := admit.Input(protocol.Input{ID: "a", Parts: []protocol.Part{blob("image/png", png())}}); err != nil {
		t.Errorf("Input = %v, want an attachment alone to pass", err)
	}
}
