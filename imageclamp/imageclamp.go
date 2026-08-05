// Package imageclamp is a shared, provider-agnostic normalization pass over a
// canonical message history that keeps an oversized image from permanently
// wedging a session.
//
// # The poison it heals
//
// A provider caps the pixel dimensions of an image it will accept (Bedrock and
// the Anthropic API both reject any side larger than 8000px with HTTP 400 "At
// least one of the image dimensions exceed max allowed size: 8000 pixels").
// An oversized image — a full-page screenshot, say — is persisted into the
// durable session transcript before it is ever sent, so once one lands, EVERY
// later turn retranscodes it, re-sends it, and 400s again. provider/errors.go
// classifies that 400 as ErrKindPermanent (fail fast), but nothing removed or
// repaired the poison, so the session stayed wedged — and on a fleet box the
// transcript lives on the durable volume and is re-adopted on respawn, so it
// survived a restart too (incident 2026-07-30, three Neptune boxes).
//
// # Why it lives at the transcode layer, shared
//
// Transcoding is stateless: every request rebuilds the provider wire format
// from the canonical history from scratch (see each provider's
// transcodeRequest). Running the clamp there means a transcript that already
// contains an oversized image now produces a VALID request on the very next
// build — no migration, no rewrite of the stored log, healing on respawn for
// free. It is deliberately the same shape as message.ResolveOrphanToolCalls,
// the other canonical-layer defense-in-depth pass every transcoder calls at
// the top of transcodeRequest against a different poisoned-history class.
//
// Clamp is read-only with respect to its input (copy-on-write): it never
// mutates a caller's stored messages, and it returns the input slice
// unchanged when nothing needed clamping, so the common path allocates
// nothing and an unchanged history still retranscodes byte-identically.
//
// # Cost and bounds
//
// Healing is not a one-time repair. Because the durable log is never
// rewritten, Clamp runs on every request build, so an oversized image is
// re-decoded, resampled, and re-encoded on EVERY subsequent turn until it
// falls out of the context window — not once. The re-encode is deterministic
// (same source bytes -> same clamped bytes), so a clamped image stays
// prompt-cache-stable turn to turn despite being recomputed. The realistic
// incident image (100x8500, sub-megapixel) is cheap, but the cost recurs per
// turn and is NOT serialized across sessions: each concurrent request build
// can hold up to maxDecodePixels of decoded RGBA (~384 MB) at once, so many
// sessions each carrying a near-limit image could pressure a fleet box's
// memory. A bounded-concurrency decode (a package-level semaphore) or a
// tiled/streaming resample is the natural follow-up if that pressure ever
// materializes; v1 keeps it simple because the incident-class image is small.
//
// The memory guards below (absurdDimension, maxDecodePixels) deliberately DROP
// an image past the bound to a text placeholder rather than downscaling it. A
// very tall full-page capture — over 30000px on a side, or over ~96M px total
// (e.g. 2560x40000) — therefore heals the wedge but loses its pixels to the
// model, even though downscaling the result to the 7680px target would have
// been safe to EMIT; the only obstacle is decoding the source at full
// resolution into memory. This is a v1 memory bound, not a fundamental limit:
// a tiled/streaming downscale that resamples such images instead of dropping
// them is the natural follow-up so that healing does not also blind the model.
package imageclamp

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif" // register GIF decoder
	"image/jpeg"  // JPEG decode (registration) + re-encode
	"image/png"   // PNG decode (registration) + re-encode
	"strings"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register WebP decoder (no encoder exists)

	"github.com/majorcontext/harness/message"
)

const (
	// absurdDimension: any image declaring a single side larger than this in
	// its header is replaced by a placeholder WITHOUT ever decoding its
	// pixels. Guards against a pathological or hostile header. See the package
	// doc "Cost and bounds": this drops (does not downscale) very tall
	// captures, a deliberate v1 memory bound.
	absurdDimension = 30000

	// maxDecodePixels bounds the total pixel area we will decode into memory.
	// An 8000x8000 RGBA image is ~256MB; this ceiling (~384MB of RGBA) lets
	// realistically-oversized screenshots through to downscaling while
	// refusing a decode bomb (a 20000x20000 image would be 1.6GB). An image
	// past this ceiling but under absurdDimension per side still becomes a
	// placeholder, never a decode — see the package doc "Cost and bounds" for
	// why this drops rather than downscales, and the follow-up it invites.
	maxDecodePixels = 96_000_000

	// The downscale target is a fraction of the provider cap, leaving margin
	// below the hard limit: 8000 * 96 / 100 = 7680.
	targetNumer = 96
	targetDenom = 100
)

// dimClass is what to do with an image of known header dimensions.
type dimClass int

const (
	classKeep      dimClass = iota // within cap: emit unchanged
	classDownscale                 // over cap but safe to decode: downscale
	classDrop                      // too large to decode safely: placeholder
)

// classify decides an image's fate from its header dimensions alone, without
// decoding pixels. maxDim is the provider's per-side cap.
func classify(w, h, maxDim int) dimClass {
	if w > absurdDimension || h > absurdDimension {
		return classDrop
	}
	if w*h > maxDecodePixels {
		return classDrop
	}
	if w > maxDim || h > maxDim {
		return classDownscale
	}
	return classKeep
}

// fitWithin returns the largest w'xh' with the same aspect ratio as wxh whose
// longer side is at most target. The shorter side is floored to 1 so a very
// long, thin image never collapses to a zero dimension.
func fitWithin(w, h, target int) (int, int) {
	if w <= target && h <= target {
		return w, h
	}
	if w >= h {
		nh := h * target / w
		if nh < 1 {
			nh = 1
		}
		return target, nh
	}
	nw := w * target / h
	if nw < 1 {
		nw = 1
	}
	return nw, target
}

// Clamp returns a view of msgs in which every inline image whose pixel
// dimensions exceed maxDim is downscaled to fit (aspect preserved), and every
// image that cannot be safely measured or decoded is replaced by a short text
// placeholder. Everything else — text, tool calls, non-image blobs, URL-only
// blobs, and images already within maxDim — is carried through unchanged, and
// the pass recurses into tool-result content (where a browser-tool screenshot
// commonly lands). The input is never mutated; when nothing needs clamping,
// msgs itself is returned.
func Clamp(msgs []message.Message, maxDim int) []message.Message {
	var out []message.Message
	for i := range msgs {
		newParts, changed := clampParts(msgs[i].Parts, maxDim)
		if !changed {
			if out != nil {
				out = append(out, msgs[i])
			}
			continue
		}
		if out == nil {
			out = make([]message.Message, i, len(msgs))
			copy(out, msgs[:i])
		}
		m := msgs[i]
		m.Parts = newParts
		out = append(out, m)
	}
	if out == nil {
		return msgs
	}
	return out
}

// clampParts applies the clamp to a parts slice, recursing into tool results.
// It returns the input slice and false when nothing changed (copy-on-write).
func clampParts(parts message.Parts, maxDim int) (message.Parts, bool) {
	var out message.Parts
	for i, p := range parts {
		var np message.Part
		switch v := p.(type) {
		case *message.Blob:
			np = normalizeBlob(v, maxDim)
		case *message.ToolResult:
			newContent, changed := clampParts(v.Content, maxDim)
			if changed {
				tr := *v
				tr.Content = newContent
				np = &tr
			} else {
				np = v
			}
		default:
			np = p
		}
		if np != p && out == nil {
			out = make(message.Parts, i, len(parts))
			copy(out, parts[:i])
		}
		if out != nil {
			out = append(out, np)
		}
	}
	if out == nil {
		return parts, false
	}
	return out, true
}

// normalizeBlob returns the clamped form of a single blob part: the same blob
// (unchanged) for a compliant image / non-image / URL blob, a new downscaled
// blob for an oversized-but-decodable image, or a text placeholder for one
// that is undecodable or too large to decode safely.
func normalizeBlob(b *message.Blob, maxDim int) message.Part {
	// Only inline image bytes are measurable here. A URL-referenced image
	// can't be fetched to measure (no network on the transcode path), and a
	// non-image blob (a PDF) has no dimensions — both pass through untouched.
	if len(b.Data) == 0 || !strings.HasPrefix(b.MediaType, "image/") {
		return b
	}

	cfg, srcFormat, err := image.DecodeConfig(bytes.NewReader(b.Data))
	if err != nil {
		return placeholder(fmt.Sprintf("[image dropped: undecodable or unsupported image data (%d bytes)]", len(b.Data)))
	}

	switch classify(cfg.Width, cfg.Height, maxDim) {
	case classKeep:
		return b
	case classDrop:
		return placeholder(fmt.Sprintf("[image dropped: %dx%d exceeds provider limit]", cfg.Width, cfg.Height))
	}

	// classDownscale: decode pixels (bounded by classify above), resample,
	// and re-encode. This full decode + resample + re-encode runs on every
	// request build for the life of the session (the durable log is never
	// rewritten), so it recurs per turn and is not serialized across sessions
	// — see the package doc "Cost and bounds".
	img, _, err := image.Decode(bytes.NewReader(b.Data))
	if err != nil {
		return placeholder(fmt.Sprintf("[image dropped: %dx%d undecodable image (%d bytes)]", cfg.Width, cfg.Height, len(b.Data)))
	}
	tw, th := fitWithin(cfg.Width, cfg.Height, maxDim*targetNumer/targetDenom)
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Over, nil)

	data, mediaType, err := encode(dst, srcFormat)
	if err != nil {
		return placeholder(fmt.Sprintf("[image dropped: %dx%d re-encode failed]", cfg.Width, cfg.Height))
	}
	return &message.Blob{MediaType: mediaType, Data: data}
}

// encode re-encodes a downscaled image, preserving JPEG (lossy source) and
// falling back to PNG for every other format. WebP has no encoder in
// golang.org/x/image, and re-quantizing a GIF palette is not worth it, so both
// become PNG (lossless).
func encode(img image.Image, srcFormat string) ([]byte, string, error) {
	var buf bytes.Buffer
	if srcFormat == "jpeg" {
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
			return nil, "", err
		}
		return buf.Bytes(), "image/jpeg", nil
	}
	if err := png.Encode(&buf, img); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), "image/png", nil
}

func placeholder(text string) *message.Text {
	return &message.Text{Text: text}
}
