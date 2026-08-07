package message

import (
	"fmt"
	"strings"
)

// wireNormalizePrefix marks the synthetic message IDs NormalizeForWire mints
// for a newly inserted RoleTool message. It is deliberately distinct from
// SyntheticOrphanIDPrefix: a message built here can carry a RELOCATED REAL
// ToolResult, not only a synthesized one, and NormalizeForWire's output is
// never persisted or replayed (see its doc comment), so it needs no relation
// to IsSyntheticOrphanID's compact-record guard.
const wireNormalizePrefix = "wire-normalized-"

// transcodeSpan is a maximal span of consecutive ORIGINAL messages sharing
// one side (assistant or not). This is NOT borrowed from the oracle's own
// wireRun/foldRuns (message/wire_oracle_test.go): it models
// provider/anthropic/transcode.go's own merge step ("The API requires
// strict user/assistant alternation; merge adjacent same-role messages",
// transcodeRequest's same-role merge) — the actual, already-shipped code
// that decides which canonical messages land in one wire turn. The oracle
// and this file both model that same external, independently-checkable
// fact because any correct model of it must; neither derives it from the
// other, and this file never imports or calls anything in a _test.go file.
// Where the two genuinely diverge is what they DO with a span: the oracle
// only tallies per-id counts and reports a mismatch (foldRuns/checkWire);
// this file additionally decides WHICH real ToolResult answers WHICH
// tool_use, in what order, and where in the OUTPUT slice to place a
// relocated or synthesized one — decisions the oracle never has to make at
// all, since it only ever inspects a candidate output, never builds one.
// See NormalizeForWire's own doc comment ("Relocation safety") for that
// machinery, and this package's golden test against the REAL anthropic
// transcoder (provider/anthropic/transcode_test.go,
// TestTranscodeSplitAssistantMessageRelocatesRealResult) for independent
// proof this span model matches the shipped merge code on the shapes that
// test covers.
//
// # This model is not exact — NEP-5304
//
// "Models the merge step" above is not "is read directly off it": on one
// input shape the two diverge, and computeTranscodeSpans's own doc comment
// states the divergence precisely. The verified impact, confirmed by
// running both the pre-stack and current anthropic transcoders against the
// same input: for `[user(ToolCall X), assistant(foreign reasoning only),
// tool(ToolResult X)]`, the pre-stack (main) anthropic transcoder paired
// `tool_use(X)` and `tool_result(X)` as adjacent blocks in one valid wire
// message — the dropped assistant message left them touching. This
// package's span model instead sees TWO separate non-assistant runs, so
// demoteWireInvalidToolResults treats both the real ToolCall and the real
// ToolResult as unanswered and rewrites BOTH to plain text. That is worse
// fidelity than main on this one shape.
//
// No byte is lost — every real value survives, readable, as text — and no
// session wedges. This is fidelity loss, never a wedge, never data loss.
// The common shape this whole file exists for — a ToolCall properly inside
// an assistant message — is unaffected and strictly better than main:
// NormalizeForWire relocates and repairs shapes main never touched at all.
// NEP-5304 tracks the fix: fold a zero-block message into its neighbor's
// span instead of starting a new one.
type transcodeSpan struct {
	assistant        bool
	msgStart, msgEnd int
}

// computeRelocationBarrier returns, for every entry in allResults (already
// in seq/document order), the largest run index a relocation of that entry
// may ever target: barrierAfter[idx] is simply the ORIGIN run of the very
// next real result in document order (or "no limit" for the last one).
//
// # Why this one number is enough
//
// A result's FINAL run is always >= its own origin run: relocation only
// ever moves a result FORWARD (see NormalizeForWire's "Relocation safety"),
// and a result that is never relocated stays at its origin. So for any
// result Y, origin(Y) <= final(Y) always holds, with no further case
// analysis needed — it does not matter whether Y ends up kept in place,
// claimed from the pool, or force-relocated out of an assistant block.
//
// Now chain that fact forward. If every result X is only ever relocated to
// a target T with T <= origin(nextResult(X)), then by that same fact
// applied to nextResult(X) itself, T <= origin(nextResult(X)) <=
// final(nextResult(X)). Since X's own final position is exactly its
// (possibly relocated) target, final(X) <= final(nextResult(X)) for every
// consecutive pair — and a relation that holds for every consecutive pair
// in a sequence holds for the whole sequence by induction: the full final
// order can never invert relative to the original seq order, for ANY two
// results, adjacent or not, same id or different.
//
// # Why two narrower, id-keyed guards attempted first were not enough
//
// Earlier attempts guarded per CALL ID — "is total demand for this id
// enough to drain every deposit," "is a same-id sibling kept in place
// nearby" — and each closed one live counterexample from this package's
// own property test (TestNormalizeForWirePropertyNoDataLoss) only for
// rapid to find a next one: temporal ordering within an id (demand
// resolved by synthesis before its answering deposit even existed, so a
// same-id total looked "fully drained" when it was not), and cross-id
// interaction (an unrelated id's legitimate claim jumping past a
// DIFFERENT id's real result that was never a contender for the same pool
// slot at all, so no per-id accounting was ever going to see it). Chasing
// each shape individually kept finding another one because id was never
// the right axis to guard on — POSITION is: regardless of id, nothing may
// ever move past the fixed lower bound the very next real result already
// establishes. This single per-index rule subsumes both: nothing about it
// depends on which id anything carries.
func computeRelocationBarrier(msgRun []int, allResults []wireResultOcc) []int {
	const noBarrier = int(^uint(0) >> 1) // math.MaxInt, kept local: no other file needs it
	barrierAfter := make([]int, len(allResults))
	if len(allResults) == 0 {
		return barrierAfter
	}
	for i := 0; i < len(allResults)-1; i++ {
		barrierAfter[i] = msgRun[allResults[i+1].msgIdx]
	}
	barrierAfter[len(allResults)-1] = noBarrier
	return barrierAfter
}

// computeTranscodeSpans partitions messages into maximal spans sharing one
// side (assistant or not), keyed purely on Message.Role — see transcodeSpan
// above for what a span models and why.
//
// Known divergence — NEP-5304: transcodeRequest drops a message that
// transcodes to zero blocks (an assistant turn whose only content is
// another provider's reasoning) and opens no wire message for it, merging
// the runs on either side. computeTranscodeSpans does not know this: a
// zero-block assistant message still starts its own span here, so it is a
// span boundary to this function though it is invisible on the wire. See
// transcodeSpan's own doc comment for the verified, worse-than-main impact
// this causes on one input shape.
func computeTranscodeSpans(messages []Message) []transcodeSpan {
	var spans []transcodeSpan
	for i, m := range messages {
		isAssistant := m.Role == RoleAssistant
		if len(spans) == 0 || spans[len(spans)-1].assistant != isAssistant {
			spans = append(spans, transcodeSpan{assistant: isAssistant, msgStart: i, msgEnd: i})
			continue
		}
		spans[len(spans)-1].msgEnd = i
	}
	return spans
}

// wireResultOcc is one real ToolResult's location and identity, as found by
// NormalizeForWire's initial scan. content is the RAW Content field
// (never SafeContent) so a caller comparing against checkNoDataLoss's own
// raw-Content snapshot sees byte-identical values for anything left
// untouched or relocated; NormalizeForWire assumes its input has already
// been through Message.Normalize for the empty-content invariant, exactly
// as every real call site (the three transcoders, all of which already
// Normalize on ingest) guarantees.
type wireResultOcc struct {
	msgIdx, partIdx int
	callID          string
	isError         bool
	content         Parts
}

// partKey identifies one Part's original position for removal tracking.
type partKey struct{ msgIdx, partIdx int }

// demoteWireInvalidToolResults returns messages with every ToolResult or
// ToolCall that is STILL wire-invalid — after every relocation and
// synthesis this file's main pass can perform — rewritten to an ordinary
// Text part. A ToolResult is demoted when it sits in an assistant-role wire
// block (invariant 2's tool_result half, unconditionally, no exception) or
// is surplus for its run (more occurrences of an id than that run's demand
// needs, invariant 4's surplus direction, including a CallID with literally
// zero ToolCalls anywhere, which is always surplus everywhere it could
// possibly sit). A ToolCall is demoted, unconditionally, whenever it sits
// in a non-assistant run (invariant 2's tool_use half) — no ToolCall there
// is ever counted as legitimate demand for a ToolResult, so answering it
// (the gap's original fix) is not enough; the misplaced tool_use block
// itself must be demoted too.
//
// # Why a POST-pass, checked against the oracle's own invariants, not a
// # narrower pre-pass keyed on "no ToolCall anywhere"
//
// An earlier version of this function only handled the zero-ToolCall-
// anywhere case, run BEFORE relocation. That missed a real, DIFFERENT
// shape TestNormalizeForWirePropertyWireValid's fully arbitrary
// generator found: two real results answering DIFFERENT ids both sitting
// in one assistant message. computeRelocationBarrier correctly refuses to
// relocate the first past the second (relocating it would reorder them),
// which is safe, but leaves it stuck failing invariant 2 — and the
// SECOND's own forced relocation (the assistant-origin leftover branch
// below NormalizeForWire's main loop) can equally end up landing in a run
// with zero demand for it, failing invariant 4's surplus direction
// instead. Neither is the "no ToolCall anywhere" shape — both ids are
// real and answerable in principle — but the barrier that keeps
// checkNoDataLoss provably safe (see computeRelocationBarrier's own doc
// comment) does not promise every result a spot the ORACLE calls valid,
// only that no relocation it approves can ever reorder two real results.
// Checking wire-validity directly, on the ACTUAL final structure, after
// every relocation decision has already been made, catches every shape
// that falls out of that gap in one pass, including the original
// zero-ToolCall-anywhere one — a stray with no ToolCall anywhere is
// ALWAYS surplus, in every run, by definition — so this replaces that
// narrower pre-pass rather than running alongside it.
//
// # Why demotion is still safe here, not just for the original shape
//
// Nothing this pass demotes was ever a legitimate answer to any demand, and
// nothing it demotes was ever a legitimate call either: checkWire's own
// invariants (message/wire_oracle_test.go) are exactly the definition of
// "this tool_result or tool_use block can never be valid here, no matter
// what". Changing its TYPE — not deleting it, not moving it again — keeps
// every byte of the real Content/IsError state (see demoteToolResult) or
// CallID/Name/Arguments (see demoteToolCall) plainly visible to the model
// as ordinary text, while removing it from the tool_use/tool_result
// adjacency accounting entirely. This runs strictly AFTER NormalizeForWire's
// own relocation pass returns, over its output, never touching input.
//
// # Why a demoted Text part is never left inside a RoleTool message
//
// A demoted part loses every reason to sit in a RoleTool message: it is no
// longer a ToolResult (or, for a demoted ToolCall, no longer a ToolCall), so
// it answers no tool_use, proposes none either, and needs no
// tool_call_id-addressed wire slot. Leaving it there anyway (mutating only
// the Part, never the Message.Role, as an earlier version of this function
// did) is a real, transcode-time regression: provider/openaicompat's own
// transcodeToolMessages is role-strict and hard-errors on any non-ToolResult
// part in a "tool"-role message, so the exact orphan-tool_result wedge this
// function exists to fix turned into a total request-BUILD failure on that
// provider (see PR #108's finding 1, and
// provider/openaicompat/transcode_test.go's
// TestTranscodeOrphanToolResultBuildsSuccessfully, which drives the REAL
// transcoder — a canonical-slice-only check, like this package's own
// property tests, cannot see a provider's own role-strictness at all).
// anthropic and the OpenAI Responses adapter are both role-agnostic (a Text
// part is valid in any wire role there), so demoting in place never wedged
// them — but the fix below does not special-case openaicompat: it hoists
// a demoted part into a new, adjacent RoleUser message instead, which every
// transcoder in this package accepts, keeping the canonical output
// uniformly valid rather than provider-specific. A RoleAssistant message's
// demoted LABEL TEXT stays demoted IN PLACE: a Text part is already valid
// there on every transcoder (an assistant wire turn's content is exactly
// the union of its parts, role-agnostic same as RoleUser), and this is
// invariant 2's OWN violation this function exists to repair — a
// tool_result can never legally sit in an assistant-role wire block, but
// ordinary text always can. A demoted result's BLOB is different and is
// NEVER left in place, even here: provider/openaicompat's
// transcodeAssistantMessage rejects any Blob outright, and even where a
// transcoder's own code has no such check (anthropic's transcodeBlob is
// role-agnostic), the Anthropic Messages API itself rejects an image block
// in an assistant turn — images are user-turn only (PR #108 round 5's
// finding on this line). demoteToolResult's own doc comment and
// buildSafeBlob cover the split in full; see demoteWireInvalidToolResults'
// own rebuild loop below for where a surviving Blob is hoisted to instead.
//
// # Why the hoisted message lands after the whole RUN, not after one message
//
// A first attempt at the hoist above inserted the new message immediately
// after the single canonical message the demoted part came from. That is
// wrong whenever that message is not alone in its wire run: two consecutive
// RoleTool messages both answering one assistant's tool_calls (the
// legitimate "results split across two consecutive RoleTool messages"
// shape — see NormalizeForWire's own gap enumeration) are ONE wire run
// under this file's model (computeTranscodeSpans groups every consecutive
// non-assistant message together, regardless of which non-assistant Role
// each one carries). Splicing a new message after only the FIRST of the
// two strands the demoted text BETWEEN them — provider/openaicompat's
// chat/completions wire requires every "tool" message answering a given
// assistant's tool_calls to be contiguous and to directly follow it, so an
// interleaved non-"tool" message breaks that association. The request
// still BUILDS (Text is valid in a RoleUser message), so this was not
// caught by a build-only check — it produces a request the PROVIDER then
// rejects with an asynchronous 400 at request time, which is the exact
// wedge class this whole line of work exists to remove, just moved later.
// See provider/openaicompat/transcode_test.go's
// TestTranscodeOrphanToolResultDoesNotSplitContiguousToolRun, which drives
// the real transcoder over exactly this two-consecutive-RoleTool-messages
// shape.
//
// The fix: gather every demoted part found ANYWHERE across a whole
// non-assistant RUN (computeTranscodeSpans' own run boundaries — the same
// abstraction NormalizeForWire's own prepend table already keys off of)
// into one message, and place it after the run's LAST message
// (runs[ri].msgEnd), never after an individual message inside it. Real,
// kept parts stay in their original message and position — only the
// demoted ones move, and they move exactly once, past every other message
// in their own run, never past a message in a DIFFERENT run. Order among
// multiple demoted parts is preserved: they are collected in strict
// document order (by message, then by part) as the run is scanned, and
// runs themselves are processed in document order, so a demoted part from
// an earlier run is always emitted before one from a later run.
func demoteWireInvalidToolResults(messages []Message) []Message {
	runs := computeTranscodeSpans(messages)
	msgRun := make([]int, len(messages))
	for ri, r := range runs {
		for i := r.msgStart; i <= r.msgEnd; i++ {
			msgRun[i] = ri
		}
	}
	// useByRun only ever collects demand from an ASSISTANT run's own
	// ToolCalls: a ToolCall sitting in a non-assistant run is never a
	// legitimate tool_use no matter what answers it (invariant 2's
	// symmetric tool_use half, message/wire_oracle_test.go) — it is
	// unconditionally demoted below, never counted as demand for a
	// ToolResult in its own or any other run.
	useByRun := make([][]string, len(runs))
	for i := range messages {
		ri := msgRun[i]
		if !runs[ri].assistant {
			continue
		}
		for _, p := range messages[i].Parts {
			if tc, ok := p.(*ToolCall); ok {
				useByRun[ri] = append(useByRun[ri], tc.CallID)
			}
		}
	}

	toDemote := make(map[partKey]bool)
	var carry []string
	for ri, r := range runs {
		if r.assistant {
			for i := r.msgStart; i <= r.msgEnd; i++ {
				for pi, p := range messages[i].Parts {
					if _, ok := p.(*ToolResult); ok {
						toDemote[partKey{i, pi}] = true
					}
				}
			}
			carry = useByRun[ri]
			continue
		}
		// Every ToolCall in a non-assistant run is unconditionally
		// wire-invalid regardless of whether anything answers it — see this
		// function's doc comment.
		for i := r.msgStart; i <= r.msgEnd; i++ {
			for pi, p := range messages[i].Parts {
				if _, ok := p.(*ToolCall); ok {
					toDemote[partKey{i, pi}] = true
				}
			}
		}
		demandCount := make(map[string]int, len(carry))
		for _, id := range carry {
			demandCount[id]++
		}
		carry = nil
		for i := r.msgStart; i <= r.msgEnd; i++ {
			for pi, p := range messages[i].Parts {
				tr, ok := p.(*ToolResult)
				if !ok {
					continue
				}
				if demandCount[tr.CallID] > 0 {
					demandCount[tr.CallID]--
				} else {
					toDemote[partKey{i, pi}] = true
				}
			}
		}
	}
	// A trailing assistant run's own leftover carry demands nothing that
	// could still be sitting, undemoted, in a message: any ToolResult that
	// could ever have satisfied it was already visited (and, if it
	// qualified, marked) by one of the two branches above — every
	// ToolResult in messages lives in SOME run, assistant or not.

	if len(toDemote) == 0 {
		return messages
	}
	out := make([]Message, 0, len(messages))
	for ri, r := range runs {
		if r.assistant {
			// A demoted label Text is already valid right where it sits in
			// an assistant-role wire block on every transcoder — see this
			// function's own doc comment for why only a non-assistant run
			// needs the run-boundary hoist below. A demoted Blob is
			// DIFFERENT: it must NEVER be left in a RoleAssistant message
			// (openaicompat's transcodeAssistantMessage rejects any Blob
			// outright, and even where a transcoder's own code has no such
			// check — anthropic's transcodeBlob is role-agnostic — the
			// Anthropic Messages API itself rejects an image block in an
			// assistant turn; images are user-turn only). So a
			// buildSafeBlob surviving this demotion is collected across the
			// WHOLE run, same as the non-assistant branch below, and hoisted
			// into its own RoleUser message after the run's last message —
			// never spliced into the run itself.
			var hoistedBlobs Parts
			for i := r.msgStart; i <= r.msgEnd; i++ {
				m := messages[i]
				var newParts Parts
				changed := false
				for pi, p := range m.Parts {
					tr, ok := p.(*ToolResult)
					if ok && toDemote[partKey{i, pi}] {
						label, blobs := demoteToolResult(tr)
						newParts = append(newParts, label)
						hoistedBlobs = append(hoistedBlobs, blobs...)
						changed = true
					} else {
						newParts = append(newParts, p)
					}
				}
				if changed {
					nm := m
					nm.Parts = newParts
					out = append(out, nm)
				} else {
					out = append(out, m)
				}
			}
			if len(hoistedBlobs) > 0 {
				out = append(out, Message{
					ID:    fmt.Sprintf("%s%d-demoted-blobs", wireNormalizePrefix, ri),
					Role:  RoleUser,
					Parts: hoistedBlobs,
				})
			}
			continue
		}

		// Non-assistant run: pull every demoted part OUT of wherever it
		// sits, across every message in this WHOLE run, and collect them
		// into one message placed AFTER the run's own last message — never
		// between two of the run's own messages (see this function's doc
		// comment, "Why the hoisted message lands after the whole RUN").
		// A demoted part is either a surplus ToolResult or a ToolCall that
		// can never be wire-valid here (every ToolCall in a non-assistant
		// run — see the unconditional marking loop above); non-demoted
		// parts (a real, still-answered ToolResult) keep their original
		// message and position untouched.
		var demoted Parts
		for i := r.msgStart; i <= r.msgEnd; i++ {
			m := messages[i]
			var kept Parts
			hasDemoted := false
			for pi, p := range m.Parts {
				if !toDemote[partKey{i, pi}] {
					kept = append(kept, p)
					continue
				}
				switch v := p.(type) {
				case *ToolResult:
					hasDemoted = true
					label, blobs := demoteToolResult(v)
					demoted = append(demoted, label)
					demoted = append(demoted, blobs...)
				case *ToolCall:
					hasDemoted = true
					demoted = append(demoted, demoteToolCall(v))
				default:
					// toDemote is only ever set for a ToolResult or
					// ToolCall part above; keep any other part type
					// unchanged rather than silently dropping it.
					kept = append(kept, p)
				}
			}
			if !hasDemoted {
				out = append(out, m)
				continue
			}
			if len(kept) > 0 {
				nm := m
				nm.Parts = kept
				out = append(out, nm)
			}
		}
		if len(demoted) > 0 {
			out = append(out, Message{
				ID:    fmt.Sprintf("%s%d-demoted", wireNormalizePrefix, ri),
				Role:  RoleUser,
				Parts: demoted,
			})
		}
	}
	return out
}

// buildSafeBlob reports whether b can survive demotion as a REAL wire Blob
// part without failing to BUILD on any of this package's three
// transcoders, in the ONE position a demoted Blob is ever placed: a
// RoleUser message (see demoteWireInvalidToolResults's own doc comment —
// a survivingBlob is always hoisted there, never left in a RoleAssistant
// one). Derived from reading each transcoder's own Blob handling, not
// assumed:
//
//   - provider/anthropic/transcode.go's transcodeBlob (~line 277) accepts
//     ANY MediaType — image/* becomes an "image" block, anything else a
//     "document" block — as long as Data or URL is present; a blob with
//     neither errors "blob has neither data nor url".
//   - provider/openai/transcode.go's transcodeBlob (~line 298) accepts
//     image/* (Data or URL) or application/pdf (Data only — a PDF by URL
//     errors "is not supported"); anything else errors "unsupported blob
//     media type".
//   - provider/openaicompat/transcode.go's blobURL (~line 343) accepts
//     ONLY image/* (Data or URL); anything else — including
//     application/pdf, which openai alone tolerates — errors "unsupported
//     blob media type". This is the narrowest of the three and forces the
//     intersection below: no PDF (openaicompat has no wire form for one at
//     all), and never a data-less, URL-less blob (every transcoder errors
//     on that regardless of media type).
func buildSafeBlob(b *Blob) bool {
	return strings.HasPrefix(b.MediaType, "image/") && (len(b.Data) > 0 || b.URL != "")
}

// demoteToolResult renders tr as a label Text — preserving every byte of
// its real Content's TEXT and its IsError flag in readable form — plus
// any of tr's own Blobs that are buildSafeBlob, returned separately so the
// caller can hoist them (see demoteWireInvalidToolResults's own doc
// comment for why changing TYPE, not deleting or dropping bytes, is the
// only valid repair for a tool_result that can never be wire-valid
// wherever it sits).
//
// # Why a non-build-safe Blob is note-flattened, not kept as a real Part
//
// An earlier version of this function kept EVERY Blob as a real Part
// (PR #108's `02a0fa6`, fixing an anthropic-only fidelity loss where a
// bare "[N image attachment(s) omitted]" note replaced a tool_result
// Blob that anthropic transcodes to a genuine wire "image" block). That
// went too far the other way: a demoted Blob is always hoisted into (or
// left in) a RoleUser message, and provider/openai and
// provider/openaicompat both hard-error building ANY request containing a
// non-image/*, or data-less/URL-less, Blob there (see buildSafeBlob's own
// doc comment) — turning the orphan/surplus-tool_result wedge this whole
// file exists to fix back into a total request-BUILD failure for exactly
// the blob-bearing shape (PR #108 round 5). A Blob that is NOT
// buildSafeBlob is therefore folded into the label's own note instead,
// naming its media type, exactly the trade provider/openai's and
// provider/openaicompat's own toolResultOutput helpers already make for
// EVERY tool-result Blob today — this file accepts the same trade for the
// narrower non-build-safe case rather than shipping a request that cannot
// build. A buildSafeBlob (the common case: a real screenshot with inline
// data or a URL) still survives byte-for-byte, keeping anthropic's
// fidelity win for that case.
func demoteToolResult(tr *ToolResult) (label *Text, survivingBlobs Parts) {
	body := tr.Content.Text()
	var droppedTypes []string
	for _, p := range tr.Content {
		b, ok := p.(*Blob)
		if !ok {
			continue
		}
		if buildSafeBlob(b) {
			survivingBlobs = append(survivingBlobs, b)
		} else {
			droppedTypes = append(droppedTypes, fmt.Sprintf("%q", b.MediaType))
		}
	}
	if len(survivingBlobs) > 0 {
		note := fmt.Sprintf("[%d image attachment(s) attached below]", len(survivingBlobs))
		if body == "" {
			body = note
		} else {
			body += "\n" + note
		}
	}
	if len(droppedTypes) > 0 {
		note := fmt.Sprintf("[%d attachment(s) omitted: %s]", len(droppedTypes), strings.Join(droppedTypes, ", "))
		if body == "" {
			body = note
		} else {
			body += "\n" + note
		}
	}
	// "unplaced", not "unknown": the call this answers may be real and
	// present elsewhere in history (demoteWireInvalidToolResults's own
	// barrier-blocked shape) or may not exist at all (the original
	// zero-ToolCall-anywhere shape) — this label makes no claim either
	// way, only that it could not be placed as a wire-valid answer.
	labelStr := fmt.Sprintf("tool result for call %q, not placed as an answer", tr.CallID)
	if tr.IsError {
		labelStr += " (reported as an error)"
	}
	return &Text{Text: fmt.Sprintf("[%s]: %s", labelStr, body)}, survivingBlobs
}

// demoteToolCall renders tc as a plain Text part, preserving its call id,
// name, and raw arguments in readable form. A ToolCall has no execution
// semantics on any provider once it sits in a non-assistant wire turn: every
// transcoder in this module only ever accepts a tool_use block inside an
// assistant-role block (invariant 2's tool_use half,
// message/wire_oracle_test.go), independent of whether a matching
// ToolResult sits right next to it. Mirrors demoteToolResult: change the
// part's TYPE, never delete it or move it again, so every byte of the real
// name/id/arguments stays plainly visible to the model.
func demoteToolCall(tc *ToolCall) *Text {
	args := "{}"
	if len(tc.Arguments) > 0 {
		args = string(tc.Arguments)
	}
	return &Text{Text: fmt.Sprintf("[tool call %q (id %q), not placed as a call]: %s", tc.Name, tc.CallID, args)}
}

// NormalizeForWire returns messages repaired so every transcoder in this
// module emits a wire-valid provider request: every tool_use is answered,
// id for id, by a tool_result in the immediately following wire RUN (the
// same run-merging model the anthropic transcoder's own adjacent-message
// merge performs — see transcodeSpan above), with no tool_result stranded
// inside an assistant-role block.
//
// # Additive vs transcode-only: the line this function sits on
//
// ResolveOrphanToolCalls is purely additive and is the function
// engine.LoadSession applies to LIVE history — it must never delete,
// reorder, or relocate a part another producer wrote (see its own doc
// comment and AGENTS.md's "A history repair that runs on live or persisted
// state is additive-only"). NormalizeForWire is its transcode-only sibling:
// every call site here builds ONE throwaway provider request and never
// touches the durable record, so it MAY relocate a real ToolResult to a
// different position in the returned slice, closing gaps
// ResolveOrphanToolCalls's strict messages[i+1] adjacency model cannot see
// (see wire_oracle_meta_test.go's TestResolveOrphanToolCallsLeaves*Unrepaired cases). It still
// never DELETES a real ToolResult — every one that goes in comes out
// somewhere, unchanged, in the same relative order among all other real
// results (see checkNoDataLoss in wire_oracle_test.go, and NEP-5293's
// account of the reverted rewrite that broke exactly this promise).
//
// # The gaps this closes (NEP-5293 part 2)
//
//  1. A duplicate call id within one assistant message (the two-tool_use
//     one-tool_result shape): fixed by counting occurrences per id per run,
//     not testing set membership.
//  2. A ToolCall sitting in a non-assistant message: a tool_use block is
//     wire-invalid on a non-assistant turn independent of whether anything
//     answers it (invariant 2's tool_use half, message/wire_oracle_test.go,
//     added after review found this gap's original fix — answering it
//     within its own run — left the misplaced tool_use itself untouched).
//     Fixed by demoteWireInvalidToolResults (below) unconditionally
//     demoting every ToolCall found in a non-assistant run to plain text,
//     the same repair already applied to a ToolResult that can never be
//     wire-valid — see that function's own doc comment.
//  3. A ToolResult preceding its ToolCall: fixed by relocating the stray
//     real result forward to sit in the run that answers its (later)
//     ToolCall, rather than leaving it in place AND adding a synthetic.
//  4. A ToolResult separated from its ToolCall by an intervening
//     same-side message: this is not actually a distinct repair — once
//     demand/supply is computed at wire-RUN granularity (this function's
//     central fix, replacing ResolveOrphanToolCalls's raw
//     messages[i+1] check), the run-merged wire is already valid and no
//     change is needed at all. The bug in the additive function is that it
//     reasons about a single next MESSAGE and, seeing a non-tool message
//     there, wrongly concludes the call is unanswered and splices in a
//     synthetic "no result" error ahead of the real one — see this
//     function's own tests for the wire dump this produces on current
//     main.
//  5. A ToolResult that can NEVER be wire-valid anywhere it could be
//     placed: relocation and counting both assume a demanding ToolCall
//     exists SOMEWHERE to place the answer next to; this one has none
//     (zero ToolCalls anywhere sharing its id), or every placement
//     relocation can prove safe (see "Relocation safety" below) still
//     leaves it in a run that does not demand it. Fixed by
//     demoteWireInvalidToolResults, a POST-pass run over this function's
//     own output: change the block's TYPE to plain text — never deleting
//     it, never moving it again — which is valid in every wire position
//     this package's transcoders ever produce. See that function's own
//     doc comment for the full account, including the live property-test
//     counterexample this closes.
//
// # Relocation safety
//
// A real result is only ever relocated FORWARD (to a later run than the
// one it was found in), and only when doing so cannot jump it past
// another real result the relative-order guarantee (computeRelocationBarrier)
// requires it stay behind. Claiming is refused — the result is left exactly
// where it is — whenever that barrier or an id mismatch blocks it, never
// by moving it and accepting the reorder. A relocation refused this way is
// not the end of the story for that result, though: demoteWireInvalidToolResults
// (below, run over this function's own output as a final pass) rewrites
// ANY ToolResult still wire-invalid after every relocation and synthesis
// decision above — whether refused by the barrier, permanently orphaned
// (no ToolCall anywhere at all), or force-relocated into a run that turns
// out not to need it — to plain text: no deletion, no further movement,
// just a part type ordinary text is always valid in. The combination is
// what makes NormalizeForWire's own wire-validity total (confirmed against
// the fully arbitrary property-test generator, message/properties_test.go's
// TestNormalizeForWirePropertyWireValid, at 500,000 rapid
// iterations): relocation handles what it can prove safe, and demotion
// closes everything relocation must decline.
func NormalizeForWire(messages []Message) []Message {
	if len(messages) == 0 {
		return messages
	}

	runs := computeTranscodeSpans(messages)
	msgRun := make([]int, len(messages))
	for ri, r := range runs {
		for i := r.msgStart; i <= r.msgEnd; i++ {
			msgRun[i] = ri
		}
	}

	// useByRun only ever collects demand from an ASSISTANT run's own
	// ToolCalls, mirroring demoteWireInvalidToolResults's own rule below: a
	// ToolCall sitting in a non-assistant run is never a legitimate
	// tool_use, so it must never be treated as demand a ToolResult can
	// satisfy, and no synthetic "answer" is ever worth manufacturing for
	// one — demoteWireInvalidToolResults demotes the misplaced call itself
	// to plain text regardless, so synthesizing an answer here would only
	// add confusing noise around a call that never belonged on the wire.
	useByRun := make([][]string, len(runs))
	var allResults []wireResultOcc
	resultsByRun := make([][]int, len(runs))

	for i := range messages {
		ri := msgRun[i]
		for pi, p := range messages[i].Parts {
			switch v := p.(type) {
			case *ToolCall:
				if runs[ri].assistant {
					useByRun[ri] = append(useByRun[ri], v.CallID)
				}
			case *ToolResult:
				idx := len(allResults)
				allResults = append(allResults, wireResultOcc{
					msgIdx: i, partIdx: pi,
					callID: v.CallID, isError: v.IsError, content: v.Content,
				})
				resultsByRun[ri] = append(resultsByRun[ri], idx)
			}
		}
	}

	barrierAfter := computeRelocationBarrier(msgRun, allResults)

	removed := make(map[partKey]bool)
	claimed := make([]bool, len(allResults))
	var pool []int // indices into allResults, ascending document order, not yet claimed

	// A relocated (claimed) real result and a synthesized one both land in
	// prepend[r], placed BEFORE run r's own first message: a pool item's
	// origin run is always STRICTLY earlier than any run that claims it
	// (a same-run claim is impossible — see claimFromPool's callers below
	// — so every claim moves an item forward across at least one run
	// boundary), and every message in an earlier run precedes every
	// message in a later run in the original document. So a claimed
	// item's original position is provably earlier than every one of the
	// target run's own pre-existing messages, and it must be placed
	// before them to keep it ordered correctly relative to a DIFFERENT
	// real result that was already validly answered in place within that
	// same target run (see this function's own tests, TestNormalizeForWire
	// IsFixedPoint and its sibling in properties_test.go, for the shape
	// that broke before this split existed — a claimed item incorrectly
	// appended after an unrelated in-place result it actually precedes).
	// A synthesized entry carries no original position at all, so sharing
	// a prepend slot with a claimed one never risks a reorder either.
	prepend := make([][]Part, len(runs)+1)

	// claimFromPool scans the WHOLE pool, not just its head: an earlier
	// version tested only pool[0], so one unclaimable head entry (a surplus
	// id nothing ever demands) permanently blocked every matching real
	// result queued behind it, forcing a fabricated is_error synthesis for
	// a call whose real answer was sitting right there (see PR #108's
	// finding 2). Scanning past a non-matching or barrier-blocked head entry
	// is safe: computeRelocationBarrier's per-INDEX guard is what actually
	// keeps relative order intact (see its own doc comment), and that check
	// is applied to whichever entry this function returns, independent of
	// where in the pool it sat or in what order the pool is scanned.
	claimFromPool := func(id string, target int) (int, bool) {
		for i, idx := range pool {
			if allResults[idx].callID != id || target > barrierAfter[idx] {
				continue
			}
			pool = append(pool[:i], pool[i+1:]...)
			return idx, true
		}
		return -1, false
	}
	claimInto := func(target, idx int) {
		claimed[idx] = true
		r := allResults[idx]
		removed[partKey{r.msgIdx, r.partIdx}] = true
		prepend[target] = append(prepend[target], &ToolResult{
			CallID: r.callID, Content: r.content, IsError: r.isError,
		})
	}
	// synthesizeInto places a synthesized "no result" entry into
	// prepend[target], mirroring claimInto's own placement rule: a
	// synthesized answer to demand CARRIED FORWARD from a preceding
	// assistant run belongs immediately after that assistant run's own
	// content (prepend, the START of the target run) — not merely
	// "somewhere in the correct run" — because at least one real
	// transcoder's wire contract needs the answer positioned immediately
	// next to its call, not just co-resident in the same merged block
	// (provider/openai/transcode_test.go's TestTranscodeResolvesOrphan
	// ToolCalls: the OpenAI Responses API's input is a flat item list with
	// no message-turn grouping at all, so "same run" is this package's
	// own abstraction, not a wire-level unit OpenAI's API recognizes —
	// appending at the run's end left an unrelated item sitting between
	// the call and its synthesized answer, a real regression this
	// caught).
	synthesizeInto := func(target int, id string) {
		prepend[target] = append(prepend[target], &ToolResult{
			CallID:  id,
			Content: Parts{&Text{Text: SyntheticOrphanResultText}},
			IsError: true,
		})
	}
	fillDemand := func(target int, order []string, count map[string]int) {
		seen := make(map[string]bool, len(order))
		for _, id := range order {
			if seen[id] {
				continue
			}
			seen[id] = true
			for count[id] > 0 {
				if idx, ok := claimFromPool(id, target); ok {
					claimInto(target, idx)
				} else {
					synthesizeInto(target, id)
				}
				count[id]--
			}
		}
	}

	var carry []string // tool_use ids demanded by the preceding assistant run
	for ri, r := range runs {
		if r.assistant {
			// A ToolResult can never validly sit in an assistant-role wire
			// block, regardless of adjacency — deposit unconditionally so
			// it is relocated (forced, below, if nothing ever claims it).
			pool = append(pool, resultsByRun[ri]...)
			carry = useByRun[ri]
			continue
		}

		// A non-assistant run's OWN ToolCalls are never legitimate demand
		// (invariant 2's tool_use half, message/wire_oracle_test.go) — see
		// useByRun's own doc comment just above: it only ever collects from
		// an ASSISTANT run, so there is no "answered within its own
		// non-assistant run" case to route supply into here. A real
		// ToolResult native to this run either answers demand CARRIED
		// FORWARD from the preceding assistant run (carryCount) or is
		// surplus, with nothing left to answer it, and joins the pool.
		carryIDs := carry
		carry = nil
		carryCount := make(map[string]int, len(carryIDs))
		for _, id := range carryIDs {
			carryCount[id]++
		}

		for _, idx := range resultsByRun[ri] {
			id := allResults[idx].callID
			if carryCount[id] > 0 {
				carryCount[id]--
			} else {
				pool = append(pool, idx)
			}
		}

		fillDemand(ri, carryIDs, carryCount)
	}

	// A trailing assistant run's demand has no following run at all; its
	// virtual "run" has no pre-existing content for prepend to be
	// relative to, but rebuildWithAdditions still renders it the same way
	// as every other target.
	if len(carry) > 0 {
		demandCount := make(map[string]int, len(carry))
		for _, id := range carry {
			demandCount[id]++
		}
		fillDemand(len(runs), carry, demandCount)
	}

	// Anything still sitting unclaimed in the pool either came from a
	// non-assistant run (a genuine surplus with nothing left to answer —
	// left exactly where it is; no tool_use anywhere can ever legitimize
	// moving it, and moving it without one would risk reordering it past
	// another real result for no benefit) or an assistant run (invariant 2
	// forbids leaving it there at all — force it into the run immediately
	// following its origin, or the virtual trailing run if none exists).
	// This still respects barrierAfter: ordinarily the immediate next run
	// has nothing between it and this item to jump past, but two real
	// results both stranded in the SAME assistant message (itself
	// off-label — see message/properties_test.go's generator comment on
	// why that input shape is still exercised) share one origin run, and
	// the first of the two would otherwise be forced past the second. In
	// that rare case the relocation is refused, same as any other
	// barrier-blocked claim — see NormalizeForWire's own "Relocation
	// safety" doc comment: even invariant 2 (no tool_result in an
	// assistant block) is not worth risking a reorder over, so the result
	// is left exactly where it is, invariant 2 unrepaired for that one
	// occurrence, rather than moved.
	for _, idx := range pool {
		if claimed[idx] {
			continue
		}
		origin := msgRun[allResults[idx].msgIdx]
		if !runs[origin].assistant {
			continue
		}
		target := origin + 1
		if target >= len(runs) {
			target = len(runs)
		}
		if target > barrierAfter[idx] {
			continue
		}
		claimInto(target, idx)
	}

	// Already-valid history (the common case: every provider request goes
	// through this function) needs no rebuild at all -- rebuildWithAdditions
	// otherwise allocates a fresh slice and copies every message for zero
	// actual changes whenever nothing was removed, relocated, or
	// synthesized. demoteWireInvalidToolResults still runs unconditionally:
	// it is a POST-pass that can find work to do (e.g. gap 5's
	// zero-ToolCall-anywhere orphan) even when the relocation pass above
	// made no changes of its own, and it already short-circuits internally
	// (its own len(toDemote)==0 check) when there is nothing for IT to do
	// either.
	rebuilt := messages
	if len(removed) > 0 || anyNonEmpty(prepend) {
		rebuilt = rebuildWithAdditions(messages, msgRun, runs, removed, prepend)
	}
	return demoteWireInvalidToolResults(rebuilt)
}

// anyNonEmpty reports whether any slot in a per-target [][]Part table (the
// shape of NormalizeForWire's own prepend) holds at least one Part.
func anyNonEmpty(byTarget [][]Part) bool {
	for _, parts := range byTarget {
		if len(parts) > 0 {
			return true
		}
	}
	return false
}

// rebuildWithAdditions materializes NormalizeForWire's decisions into a new
// []Message: every original message with any removed (relocated) parts
// filtered out, dropped entirely if that leaves it with no parts; and a new
// message carrying prepend[r]'s relocated-and/or-synthesized results
// spliced in BEFORE run r's first message. See NormalizeForWire's own
// "prepend" doc comment above for why a claimed or synthesized entry must
// land before the target run's own pre-existing content.
func rebuildWithAdditions(messages []Message, msgRun []int, runs []transcodeSpan, removed map[partKey]bool, prepend [][]Part) []Message {
	out := make([]Message, 0, len(messages))
	for i, m := range messages {
		ri := msgRun[i]
		if i == runs[ri].msgStart {
			if pre := prepend[ri]; len(pre) > 0 {
				out = append(out, Message{
					ID:    fmt.Sprintf("%s%d-real", wireNormalizePrefix, ri),
					Role:  RoleTool,
					Parts: pre,
				})
			}
		}

		var newParts Parts
		var droppedAny bool
		for pi, p := range m.Parts {
			if removed[partKey{i, pi}] {
				droppedAny = true
				continue
			}
			newParts = append(newParts, p)
		}
		if !(droppedAny && len(newParts) == 0) {
			nm := m
			if droppedAny {
				nm.Parts = newParts
			}
			out = append(out, nm)
		}
	}
	// The virtual trailing run (index len(runs)): rendered the same way as
	// every other target.
	if trailingReal := prepend[len(runs)]; len(trailingReal) > 0 {
		out = append(out, Message{ID: wireNormalizePrefix + "trailing-real", Role: RoleTool, Parts: trailingReal})
	}
	return out
}
