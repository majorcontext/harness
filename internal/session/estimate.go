package session

import "github.com/majorcontext/harness/internal/eventlog"

const (
	bytesPerToken = 4
	imageTokens   = 1600
)

// estimateTokens estimates the prompt size of history for a provider that
// reports no prompt tokens: one token for each 4 bytes of text, tool call, tool
// result, and reasoning, and a flat cost for each image.
func estimateTokens(history []eventlog.Message) int64 {
	var bytes, images int64
	for _, m := range history {
		b, i := sizeOf(m.Parts)
		bytes, images = bytes+b, images+i
	}
	return bytes/bytesPerToken + images*imageTokens
}

// sizeOf returns the bytes of the text-shaped content of parts, and the number of images.
func sizeOf(parts []eventlog.Part) (bytes, images int64) {
	for _, p := range parts {
		bytes += int64(len(p.Text) + len(p.Name) + len(p.Arguments) + len(p.CallID))
		if p.Type == eventlog.PartBlob {
			if len(p.MediaType) >= 6 && p.MediaType[:6] == "image/" {
				images++
			} else {
				bytes += int64(p.Bytes)
			}
		}
		b, i := sizeOf(p.Blobs)
		bytes, images = bytes+b, images+i
	}
	return bytes, images
}
