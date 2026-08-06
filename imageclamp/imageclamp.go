// Package imageclamp is a shared, provider-agnostic normalization pass over a
// canonical message history that keeps an oversized image from permanently
// wedging a session.
//
// # The poison it heals
//
// A provider caps the images it will accept and rejects a violation with an
// HTTP 400 that is unrecoverable at the agent layer: because the oversized
// image is persisted into the durable session transcript before it is ever
// sent, EVERY later turn retranscodes it, re-sends it, and 400s again — and on
// a fleet box the transcript lives on a durable volume re-adopted on respawn,
// so the wedge survives a restart (incident 2026-07-30, three Neptune boxes:
// full-page screenshots >8000px, Bedrock "At least one of the image dimensions
// exceed max allowed size: 8000 pixels"). provider/errors.go classifies that
// 400 as ErrKindPermanent (fail fast), but nothing removed or repaired the
// poison.
//
// Two distinct caps can wedge a session, and Clamp enforces both:
//
//   - Pixel DIMENSION. Anthropic/Bedrock hard-reject any side over 8000px.
//     (OpenAI and Gemini instead auto-resize/tile and never reject on
//     dimension, so the cap there is defensive only.) When >20 image or
//     document blocks are in one request, Anthropic applies a stricter 2000px
//     per-side cap — see Limits.ManyImageThreshold.
//   - Encoded BYTE size. Bedrock/Vertex reject a single image over 5MB
//     base64, the direct Anthropic API over 10MB — independent of dimension, so
//     a detail-dense image well under 8000px can wedge a session too. Clamp
//     re-encodes (JPEG) and, if needed, further downscales until the emitted
//     image fits Limits.MaxImageBytes.
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
// # The downscale target
//
// No provider processes an image above ~2576px on its long edge — Anthropic
// resizes to 2576px (Claude 4.7+; 1568 on older models), OpenAI's default tile
// path to ~2048px, Gemini into 768px tiles — and all three cap token cost at
// that internal resolution regardless of input size. So Limits.TargetDim is
// set to 2576px by every adapter: it is the largest resolution any model
// actually consumes, it costs the same tokens as a larger image would, and it
// keeps the emitted bytes small enough that Limits.MaxImageBytes rarely has to
// intervene. Sending more pixels than that is pure payload with no fidelity
// gain, since the model discards them.
//
// # Cost and bounds
//
// Healing is not a one-time repair. Because the durable log is never
// rewritten, Clamp runs on every request build, so an oversized image is
// re-decoded, resampled, and re-encoded on EVERY subsequent turn until it
// falls out of the context window — not once. The re-encode is deterministic
// (same source bytes -> same clamped bytes), so a clamped image stays
// prompt-cache-stable turn to turn despite being recomputed — with one
// intra-session exception: the many-image cap (Limits.ManyImageThreshold)
// makes an image's clamped size depend on the request's total image/document
// block count, so a request growing past the threshold downscales every image
// to ManyImageDim and invalidates that cache prefix once at the boundary. This
// is unavoidable and mirrors the provider's own server-side behavior (it too
// applies the stricter cap by request block count). The realistic
// incident image (100x8500, sub-megapixel) is cheap, but the cost recurs per
// turn and is NOT serialized across sessions: a single build's peak holds the
// decoded source (up to maxDecodePixels of RGBA, ~384 MB) plus the resample
// destination at once, so many concurrent such builds could pressure a fleet
// box's memory. A bounded-concurrency decode (a package-level semaphore) is the
// natural follow-up if that pressure ever materializes; v1 keeps it simple
// because the incident-class image is small, and the 2576px target keeps the
// destination tiny.
//
// The memory guards below (absurdDimension, maxDecodePixels) deliberately DROP
// an image past the bound to a text placeholder rather than downscaling it: a
// very tall capture (over 30000px on a side, e.g. 2560x40000) or a moderately
// square one over ~96M px total (roughly 9800x9800, e.g. a 10000x10000 retina
// screenshot) heals the wedge but loses its pixels to the model, since the
// obstacle is decoding the source at full resolution into memory. A
// tiled/streaming downscale that resamples such images instead of dropping
// them is the natural follow-up.
package imageclamp

import (
	"bytes"
	"encoding/base64"
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
	// placeholder, never a decode — see the package doc "Cost and bounds".
	maxDecodePixels = 96_000_000

	// byteFloor is the smallest long edge the byte-budget reducer will scale an
	// image down to while trying to fit Limits.MaxImageBytes. If an image still
	// exceeds the budget once its long edge reaches this, it is dropped to a
	// placeholder rather than shrunk into illegibility.
	byteFloor = 512

	// jpegQuality is used when re-encoding a downscaled image whose source was
	// JPEG, and when the byte-budget reducer switches an over-budget image to
	// JPEG (far smaller than PNG for photographic content).
	jpegQuality = 85
)

// Limits describes one provider's image constraints. Each adapter constructs a
// Limits next to itself (caps live with the adapter, per the transcode design)
// and passes it to Clamp.
type Limits struct {
	// MaxDim is the hard per-side pixel cap: an image with a longer side is
	// downscaled to TargetDim. Anthropic/Bedrock reject above 8000px; OpenAI
	// and Gemini auto-resize instead, so the cap is defensive there.
	MaxDim int
	// TargetDim is the long edge an oversized image is downscaled to. 2576px
	// (Claude's high-res processing edge) is the practical maximum any model
	// consumes; see the package doc "The downscale target".
	TargetDim int
	// ManyImageThreshold: when a request carries MORE than this many image
	// (and, on Bedrock/Vertex, document) blocks, a stricter per-side cap of
	// ManyImageDim applies to every image in the request. 0 disables the rule
	// (OpenAI/Gemini have no equivalent).
	ManyImageThreshold int
	ManyImageDim       int
	// MaxImageBytes caps the base64-encoded size of a single emitted image. An
	// image over budget is re-encoded as JPEG and, if still over, progressively
	// downscaled until it fits or hits byteFloor (then dropped). 0 disables the
	// byte budget (OpenAI has no per-image size limit).
	MaxImageBytes int
	// RecurseToolResults clamps images nested in tool-result content too.
	// Adapters that emit tool-result images on the wire (anthropic) set this
	// true; adapters that omit them with a text note (openai, openaicompat)
	// leave it false to skip a wasted decode/resample of an image that can
	// never reach the provider.
	RecurseToolResults bool
}

// effective is Limits after the many-image rule has been resolved for a
// specific request, threaded down the recursion.
type effective struct {
	maxDim    int
	targetDim int
	maxBytes  int
	recurse   bool
}

// dimClass is what to do with an image of known header dimensions.
type dimClass int

const (
	classKeep      dimClass = iota // within cap: dimension is fine
	classDownscale                 // over cap but safe to decode: downscale
	classDrop                      // too large to decode safely: placeholder
)

// classify decides an image's dimension fate from its header alone, without
// decoding pixels. maxDim is the effective per-side cap.
func classify(w, h, maxDim int) dimClass {
	if w > absurdDimension || h > absurdDimension {
		return classDrop
	}
	if w*h > maxDecodePixels {
		return classDrop
	}
	if maxDim > 0 && (w > maxDim || h > maxDim) {
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

// Clamp returns a view of msgs in which every inline image is brought within
// lim's dimension and byte caps — oversized images are downscaled (aspect
// preserved) and, if still too large in bytes, re-encoded/shrunk to fit — and
// every image that cannot be safely measured or decoded is replaced by a short
// text placeholder. Everything else — text, tool calls, non-image blobs,
// URL-only blobs, and images already within every cap — is carried through
// unchanged. The input is never mutated; when nothing needs clamping, msgs
// itself is returned.
func Clamp(msgs []message.Message, lim Limits) []message.Message {
	maxDim, targetDim := lim.MaxDim, lim.TargetDim
	// Resolve the many-image stricter cap once for the whole request. Count only
	// blocks that actually reach the provider: tool-result images are omitted on
	// the wire for adapters that don't recurse (openai/openaicompat), so counting
	// them would over-trip the threshold and needlessly downscale other images.
	if lim.ManyImageThreshold > 0 && lim.ManyImageDim > 0 &&
		countImageDocBlocks(msgs, lim.RecurseToolResults) > lim.ManyImageThreshold {
		if lim.ManyImageDim < maxDim || maxDim == 0 {
			maxDim = lim.ManyImageDim
		}
		if lim.ManyImageDim < targetDim || targetDim == 0 {
			targetDim = lim.ManyImageDim
		}
	}
	eff := effective{maxDim: maxDim, targetDim: targetDim, maxBytes: lim.MaxImageBytes, recurse: lim.RecurseToolResults}

	var out []message.Message
	for i := range msgs {
		newParts, changed := clampParts(msgs[i].Parts, eff)
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

// countImageDocBlocks counts the image and PDF blobs that will actually be
// emitted, for the many-image rule. On Bedrock/Vertex document blocks count
// toward the same threshold as images. It descends into tool-result content
// only when recurse is set — an adapter that omits tool-result images on the
// wire must not count them, or an unrelated image gets needlessly downscaled.
func countImageDocBlocks(msgs []message.Message, recurse bool) int {
	n := 0
	var walk func(parts message.Parts)
	walk = func(parts message.Parts) {
		for _, p := range parts {
			switch v := p.(type) {
			case *message.Blob:
				if strings.HasPrefix(v.MediaType, "image/") || v.MediaType == "application/pdf" {
					n++
				}
			case *message.ToolResult:
				if recurse {
					walk(v.Content)
				}
			}
		}
	}
	for i := range msgs {
		walk(msgs[i].Parts)
	}
	return n
}

// clampParts applies the clamp to a parts slice, descending into tool-result
// content when eff.recurse is set. It returns the input slice and false when
// nothing changed (copy-on-write).
func clampParts(parts message.Parts, eff effective) (message.Parts, bool) {
	var out message.Parts
	for i, p := range parts {
		var np message.Part
		switch v := p.(type) {
		case *message.Blob:
			np = normalizeBlob(v, eff)
		case *message.ToolResult:
			if !eff.recurse {
				np = v
				break
			}
			newContent, changed := clampParts(v.Content, eff)
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
// (unchanged) for a compliant image / non-image / URL blob, a new blob for one
// that is downscaled and/or re-encoded to fit the caps, or a text placeholder
// for one that is undecodable, too large to decode safely, or impossible to
// fit under the byte budget.
func normalizeBlob(b *message.Blob, eff effective) message.Part {
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

	class := classify(cfg.Width, cfg.Height, eff.maxDim)
	if class == classDrop {
		return placeholder(fmt.Sprintf("[image dropped: %dx%d exceeds provider limit]", cfg.Width, cfg.Height))
	}

	// Fast path: dimensions fine AND (no byte budget OR already under it) — the
	// image is emitted byte-identical, preserving the prompt-cache prefix.
	overBytes := eff.maxBytes > 0 && base64Len(len(b.Data)) > eff.maxBytes
	if class == classKeep && !overBytes {
		return b
	}

	// Decode is now required (to downscale, to re-encode smaller, or both). The
	// area guard in classify bounds this allocation. This decode + resample +
	// re-encode recurs on every request build for the life of the session (the
	// durable log is never rewritten) — see the package doc "Cost and bounds".
	img, _, err := image.Decode(bytes.NewReader(b.Data))
	if err != nil {
		return placeholder(fmt.Sprintf("[image dropped: %dx%d undecodable image (%d bytes)]", cfg.Width, cfg.Height, len(b.Data)))
	}
	if class == classDownscale {
		tw, th := fitWithin(cfg.Width, cfg.Height, eff.targetDim)
		img = scaleTo(img, tw, th)
	}

	data, mediaType, ok := fitBytes(img, srcFormat, eff.maxBytes)
	if !ok {
		return placeholder(fmt.Sprintf("[image dropped: %dx%d cannot fit under the %d-byte size limit]", cfg.Width, cfg.Height, eff.maxBytes))
	}
	return &message.Blob{MediaType: mediaType, Data: data}
}

// fitBytes encodes img and, if the result exceeds maxBytes (base64), reduces it
// to fit: first by switching to JPEG (far smaller than PNG for photographic
// content), then by progressively downscaling. Returns ok=false only if the
// image still exceeds the budget once its long edge has shrunk to byteFloor.
// maxBytes <= 0 disables the budget (the initial encode is returned as-is).
func fitBytes(img image.Image, srcFormat string, maxBytes int) ([]byte, string, bool) {
	data, mediaType, err := encode(img, srcFormat)
	if err != nil {
		return nil, "", false
	}
	if maxBytes <= 0 || base64Len(len(data)) <= maxBytes {
		return data, mediaType, true
	}

	cur := img
	for {
		jdata, jerr := encodeJPEG(cur)
		if jerr == nil && base64Len(len(jdata)) <= maxBytes {
			return jdata, "image/jpeg", true
		}
		w, h := cur.Bounds().Dx(), cur.Bounds().Dy()
		if w <= byteFloor && h <= byteFloor {
			return nil, "", false // cannot fit without shrinking below floor
		}
		nw, nh := w*4/5, h*4/5
		if nw < 1 {
			nw = 1
		}
		if nh < 1 {
			nh = 1
		}
		cur = scaleTo(cur, nw, nh)
	}
}

func scaleTo(src image.Image, w, h int) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	return dst
}

// encode re-encodes an image, preserving JPEG (lossy source) and falling back
// to PNG for every other format. WebP has no encoder in golang.org/x/image, and
// re-quantizing a GIF palette is not worth it, so both become PNG (lossless).
func encode(img image.Image, srcFormat string) ([]byte, string, error) {
	if srcFormat == "jpeg" {
		data, err := encodeJPEG(img)
		if err != nil {
			return nil, "", err
		}
		return data, "image/jpeg", nil
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), "image/png", nil
}

func encodeJPEG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// base64Len is the length of n raw bytes once base64-encoded (the size limits
// providers state are on the base64 payload, not the raw image).
func base64Len(n int) int { return base64.StdEncoding.EncodedLen(n) }

func placeholder(text string) *message.Text {
	return &message.Text{Text: text}
}
