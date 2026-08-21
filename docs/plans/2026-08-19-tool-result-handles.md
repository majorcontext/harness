# Tool-result handles

An oversized **text** tool result is retained into a per-session sidecar
store and replaced, in the canonical message, by a short preview carrying a
session-monotonic **handle** (`trh_N`). The model reads the retained bytes
back on demand with a built-in `read_tool_result` tool: a bounded range read
or a bounded literal search, never the whole blob at once.

This is an *engine-layer* feature. It changes what the canonical
`message.ToolResult` contains at the moment it is appended to history, and
nothing else. There are **zero changes to any transcoder**, to
`message.NormalizeForWire`, or to `message.ResolveOrphanToolCalls`.

## 1. Why

`bash` already caps one call's combined output at `Config.BashOutputCap`
(96 KiB, `engine/bash.go`), split as a 48 KiB head and a 48 KiB tail
(`cappedWriter`) — the tail is kept deliberately, because a FAIL line in a
build/test log is usually near the end. That cap runs **inside**
`bash.Run`, upstream of everything retention ever sees. By the time
`maybeRetainToolResult` looks at a bash result's parts, `cappedWriter` has
already discarded whatever fell in the middle.

**Corrected claim** (an earlier draft of this doc said retention fixed
bash's own truncation; that is false, and review finding F5 caught it).
Retention cannot recover bytes that never reached history in the first
place. A FAIL line sitting in the discarded middle of a run whose head and
tail were both kept is still gone — retention changes nothing about that
case. Routing bash's output through retention *before* the head/tail cap
(or removing the cap now that retention exists) is a real idea, but it is
a separate, undone follow-up; this feature does not change bash's routing
at all.

What retention actually fixes is the **uncapped** case: MCP and plugin tool
results have no cap of any kind today, destructive or otherwise. A single
large result there is not truncated at all — and because harness re-sends
full history every request (stateless transcoding), an uncapped result is
re-billed as input on every later turn for the life of the session. That
uncapped, unbounded case — not bash's already-capped one — is retention's
real value and the actual production problem this feature targets.

## 2. What lands in history

Retention rewrites the `ToolResult.Content` parts of a tool result whose
joined `*message.Text` bytes exceed `Config.ToolResultInlineBytes`. The
rewritten content is:

```
[ Text: preview header line ]
[ Text: the first N bytes of the joined text, N = ToolResultInlineBytes ]
[ ...every non-Text part of the original content, in original order ]
```

Non-`Text` parts (a `message.Blob` from `read_file`'s image path, or from
MCP's `mcpContentToParts`) are **never** retained and never dropped — they
pass through untouched, after the preview. Retention is a text-only
mechanism; image bytes are already bounded by `imageclamp.Clamp` at
transcode time.

**Known, documented reordering (review finding F14).** `splitToolResultParts`
joins *every* `Text` part into one string and collects every non-`Text` part
separately, each preserving its own relative order — but if the ORIGINAL
content interleaved them (`Text`, `Blob`, `Text`), the retained output does
not: it is always `[header, preview, ...every non-Text part]`, so a `Blob`
that originally sat *between* two `Text` parts ends up *after* their merged
preview instead. This only matters on the oversized path (retention is a
no-op otherwise, and the original order is untouched), and no built-in or
MCP producer today interleaves `Text` and `Blob` parts in one result, so
nothing observable regresses today — but a future producer that does would
see its interleaving flattened. Documented here rather than fixed, since
fixing it means preserving positional metadata retention has no reason to
carry otherwise.

### 2.1 The preview header — exact format

The header is one line, produced by exactly one function
(`engine.toolResultPreviewHeader`) so the format can never drift between the
producer and its test:

```
[tool result retained: handle=trh_1 tool=bash bytes=123456 lines=4201 preview_bytes=16384 — read the rest with read_tool_result(handle="trh_1")]
```

Field by field:

| field           | meaning                                                             |
| --------------- | ------------------------------------------------------------------- |
| `handle`        | the session-monotonic handle, `trh_<N>`, `N` starting at 1           |
| `tool`          | the name of the tool whose result was retained                       |
| `bytes`         | total bytes of the **joined** original text                          |
| `lines`         | total lines of that text (`\n` count, plus one for a non-empty tail) |
| `preview_bytes` | bytes of the preview body that follows this header                   |

The em dash is a literal U+2014. The trailing clause names the tool and the
handle in the exact call shape the model should use, because that clause is
the only in-band documentation the model gets at the moment it needs it.

A retention that is **refused** because the per-session retained-bytes cap is
already reached emits a different, equally fixed line
(`engine.toolResultCapHeader`) and no handle, since there is nothing to read
back:

```
[tool result truncated: tool=bash bytes=123456 preview_bytes=16384 — retention is exhausted for this session (the per-session retention cap has been reached); the remainder is discarded irrecoverably and no further tool result will be retained this session]
```

The wording is deliberately blunt (review finding F3(b)): the ceiling is
**monotonic** — nothing in this feature evicts or reclaims it (see §9,
below) — so this is not "try again later," it is "for the rest of this
session." A softer message here would misstate that.

## 3. Configuration

Two config keys, both `int`, both plumbed through `config.Config` into
`engine.Config` by `harness run` and `harness serve` alike.

| key                         | default   | meaning                                                    |
| --------------------------- | --------- | ---------------------------------------------------------- |
| `tool_result_inline_bytes`  | `16384`   | retain a text result larger than this; **`<= 0` disables**  |
| `tool_result_retained_bytes`| `4194304` | per-session ceiling on total retained bytes; `<= 0` disables|

`<= 0 disables` for `tool_result_inline_bytes` is the load-bearing half of
the contract: an embedder that builds a bare `engine.Config` (the zero value)
gets exactly today's behavior, byte for byte, with no sidecar directory ever
created. The config/CLI layer is what supplies the 16384 product default, the
same split `PromptRetries` already uses.

Retention also requires `Config.SessionDir`. Without a session directory there
is nowhere to durably put the bytes, and a preview pointing at a handle that
can never be read is worse than no preview: retention is off in that case,
whatever the byte limits say.

## 4. The sidecar store

```
<SessionDir>/toolresults/<session-id>/trh_<N>.txt
```

One flat file per handle, holding the joined original text verbatim — no
framing, no compression, no JSON. `read_tool_result` reads it with an
`os.File` + `bufio.Scanner`, so a range read never loads the whole file.

The store is deliberately *not* inside the session's own `.jsonl` log. The log
is append-only and is fully replayed by `LoadSession` on every resume;
inlining megabytes of retained output there would make resume pay for exactly
the bytes retention exists to keep out of memory.

## 5. Durability and resume

Every successful retention writes one durable record before the preview is
returned:

```json
{"type":"toolresult.retained","tool_result":{"handle":"trh_3","tool":"bash","bytes":123456,"lines":4201}}
```

`LoadSession` folds `toolresult.retained` records (`engine/store.go`) into
three pieces of session state:

- `toolResultNextID` advances past every handle number seen, so a resumed
  session's next handle can never collide with one already on disk. **This is
  the counter-survives-resume requirement**: handle `trh_3` in the log means a
  resumed session mints `trh_4` next, not `trh_1`.
- `toolResults[handle]` regains its metadata, so `read_tool_result` works on a
  resumed session against a handle minted by the previous process.
- `toolResultBytes` regains the running total the per-session cap is checked
  against, so the cap is a session-lifetime ceiling rather than a
  per-process one.

A record with a malformed handle, or a duplicate handle, is skipped — the same
defensive replay posture `recPromptQueued` already takes.

The record is a *pointer*, not the content. A crash between writing the
sidecar file and writing the record degrades to an orphaned file on disk and
a handle the session never knew about — wasted bytes, never wrong bytes. A
crash the other way (record written, file lost) degrades to the documented
missing-file path in §6.

## 6. `read_tool_result`

```
read_tool_result(handle, offset?, limit?, search?, max_bytes?)
```

- `handle` (required) — a `trh_N` handle from a preview header.
- `offset` — 1-based first line to return. Default 1.
- `limit` — maximum lines to return. Default 200, hard-capped at 2000.
- `search` — a **literal** substring (never a regex: a model-authored regex
  against an unbounded file is a ReDoS surface, and literal search is what
  reading a captured log actually needs). When set, `offset`/`limit` are
  ignored and the tool returns matching lines with their 1-based line
  numbers.
- `max_bytes` — output byte ceiling. Default 16384, hard-capped at 65536,
  **floored at 256** (`readToolResultMinMaxBytes`, review finding F11): below
  the floor, the fixed preamble every read writes can by itself consume the
  whole budget, leaving no room for a single line of content — a request
  under the floor is rejected outright with a clear error naming it, rather
  than silently reusing the same "no lines" wording a genuinely empty read
  produces.

Output is always bounded twice: by the line budget and by `max_bytes`,
whichever binds first, with an explicit truncation notice when either does.
Bounding is the whole point — an unbounded read back into context would defeat
retention entirely.

**Line normalization (review finding F10, documented, not fixed).** Every
read is line-oriented: `\r` is stripped from a CRLF-terminated source (Go's
`bufio.Scanner`'s default line-splitting behavior), and the emitted output
always ends in exactly one trailing `\n` regardless of whether the original
retained bytes did. A byte-exact round trip through `read_tool_result` is
therefore not guaranteed — a caller that needs the literal original bytes
back cannot get them through this tool today. Follow-up: expose a genuinely
byte-exact range mode; the F1 `io.ReaderAt` fallback below is the seed of
that (it already reads raw bytes with no line reinterpretation before
re-splitting them for its own use).

**Oversized-line fallback (review finding F1).** A single line at or beyond
the internal scan buffer (1 MiB, `readToolResultScanBuf`) defeats
`bufio.Scanner` outright — `Scan` returns `false`, `sc.Err()` is
`bufio.ErrTooLong` — and before this fix that surfaced as a plain "no
lines"/"no match" result indistinguishable from an empty one, on a result
whose own preview header told the model it was recoverable. `runReadToolResult`
now checks `sc.Err()` and, on `ErrTooLong`, retries once against a raw
`io.ReaderAt` read of the whole file with no per-line limit
(`toolResultFallbackLines`) — `ReaderAt` specifically because it is an
absolute-offset read, independent of whatever the failed `bufio.Scanner`
already consumed from the same `*os.File`'s read cursor.

Degradation, both clean and both a normal tool error (never a panic, never a
partial read):

- **Unknown handle** — the error names the handle and lists the handles this
  session actually has (bounded to the most recent 20). The handle token
  itself must be digits-only, no leading zero, no sign (review finding F13)
  — `strconv.ParseInt` alone accepts `trh_+1` and `trh_01` as valid
  spellings of `trh_1`, which the write path (`strconv.FormatInt`) never
  produces; both are rejected before any lookup, not treated as aliases.
- **Missing file** — the handle is known but its sidecar file is gone
  (an operator wiped the directory, a volume rolled back). The error says so
  and names the handle, rather than surfacing a raw `os.PathError` with an
  absolute filesystem path.

**read_tool_result's own output is exempt from retention (review finding
F2).** Without the exemption, a read whose returned text exceeds the inline
limit — the ordinary case, since the documented default `max_bytes` (16384)
sits right at a typical inline limit — mints a NEW handle instead of
returning inline, making the documented `max_bytes` ceiling unreachable in
practice and duplicating on-disk bytes for content already durably retained
under its source handle. `read_tool_result` output IS the recovery path
*for* retention; it must never re-enter it.

## 7. What this feature explicitly does not touch

`git diff` for this branch contains **no** changes under `provider/`, and no
changes to `message/wire_normalize.go` (`NormalizeForWire`) or to
`ResolveOrphanToolCalls` in `message/message.go`. Retention happens strictly
before `Session.append`, so by the time any wire-normalization or transcoding
code runs, the message it sees is an ordinary `ToolResult` carrying ordinary
`Text` parts. There is nothing for those layers to learn about handles.

This is also why retention is safe against the additive-only live-repair rule
in AGENTS.md: retention is not a *repair*. It runs once, on a message that
does not yet exist in history, at the moment the engine constructs it — the
same point `runToolCalls` already decides what the `ToolResult` contains.

## 8. Compaction interaction and the retention ceiling

The retention ceiling (`toolResultBytes` against `Config.ToolResultRetainedBytes`)
is **monotonic**: only ever incremented (`writeRetainedToolResult`), never
decremented. Nothing in this feature evicts a handle, reclaims its bytes, or
garbage-collects a sidecar file — not on read, not on compaction, not on
session end. This is a deliberate scope cut, not an oversight: see the
follow-up below.

Compaction (`engine/compact.go`) folds a contiguous prefix of turns into one
summary message, and `compactionSystemPrompt` explicitly forbids the
summarizer from transcribing tool output. Combined with the monotonic
ceiling, an early version of this feature had a real bug (review finding
F3): folding a turn whose preview line named a `trh_N` handle made that
handle **orphaned** — still on disk, still counted against the ceiling, but
no longer named anywhere in live history for the model to rediscover.

The fix is `Session.retainedResultsIndexPart` (`engine/toolresult.go`),
called from `Compact` (`engine/compact.go`) right after the summary message
is built. It appends a second, **deterministic** `message.Text` part to the
summary — one line per currently-live handle (handle, tool, byte size, a
short head excerpt), built directly from session state, never from the
LLM's summary text. That determinism is the point: a prompt-dependent
mechanism for something this load-bearing (an orphaned handle stays
permanently invisible for the rest of the session) is exactly the failure
mode being fixed. The index covers *every* handle the session currently
knows about, not only ones minted inside the folded range — a handle from
turn 1 is exactly as reachable-only-through-a-preview-line as one from the
turn just folded. Omitted entirely (no empty block) when there are no live
handles.

**Follow-up, explicitly out of scope for this PR**: no eviction or GC exists
anywhere in this feature. A long-lived session that retains enough oversized
results eventually fills `Config.ToolResultRetainedBytes` permanently — the
cap-full header (§2.1) says so honestly, but nothing ever un-fills it short
of the session ending. A real fix needs a policy (LRU? oldest-handle-first?
tied to compaction, so a handle whose ONLY reference was just folded away
becomes eligible?) that is a design decision on its own, not a one-line
addition to this PR.

**Follow-up, also out of scope**: no session-delete path exists repo-wide
today. `handleEnd` (wherever a session is torn down) never removes a
session's `.jsonl` log or its `toolresults/` sidecar directory — this
predates retention and is not new to this feature, but retention is the
first thing in this repo that puts potentially-secret bytes on disk with no
deletion path at all, which raises the stakes of that gap. Filed as a
follow-up, not fixed here.

## 9. Secrets and file permissions

A retained result is arbitrary tool output — routinely including secrets a
command printed (an env dump, a leaked credential in a log line). Review
finding F4 covered two things:

1. **File permissions.** The sidecar directory and file are now created
   `0o700`/`0o600` (were `0o755`/`0o644`) — private to the process owner,
   not group- or world-readable.
2. **Masking.** `engine/toolresult_secrets.go`'s `maskSecrets` is a
   best-effort, pattern-based redaction applied to the text before it is
   written to disk: a case-insensitive `(secret|token|password|api_key|
   access_key)[=:]\S+` match has its value replaced with `***`, key and
   separator preserved. This repo had no existing secret-masking utility to
   reuse.

**Residual risk, explicitly not fully solved.** This is a minimal pattern
matcher, not a secret scanner: it only catches the `KEY=value` /
`KEY: value` shape, only for the five listed key-name substrings, and has no
understanding of structured formats (a secret value nested inside JSON/YAML
under an unlisted key name is not caught) and no way to catch a bare token
pasted with no label at all. It also does not touch the **preview** bytes
(the first `ToolResultInlineBytes` of the joined text) — those are already
inline in the model's context by construction, independent of retention,
the same as they always were before this feature existed; only what
additionally lands on disk is masked. A secret sitting past the inline
limit is masked on disk but was still visible to the model, transiently,
in the request that produced it. A real fix (a pluggable/configurable
masking policy, or integration with a proper secret-detection library) is
follow-up work, not this PR.

## 10. Test list

| test                                                       | pins                                                            |
| ---------------------------------------------------------- | --------------------------------------------------------------- |
| `TestToolResultRetainedAboveInlineLimit`                    | over-limit text is retained; preview + handle replace it         |
| `TestToolResultPreviewHeaderExactFormat`                    | the §2.1 header, byte for byte                                   |
| `TestToolResultUnderLimitUntouched`                         | under-limit output passes through completely unchanged           |
| `TestToolResultRetentionDisabledByZeroLimit`                | `<= 0` disables; no sidecar directory is created                 |
| `TestToolResultRetentionRequiresSessionDir`                 | no `SessionDir` means no retention                               |
| `TestToolResultRetentionPreservesNonTextParts`              | a `Blob` survives retention, in order, after the preview         |
| `TestToolResultRetainedBytesCapRefusesRetention`            | the per-session cap refuses and emits the cap header             |
| `TestToolResultHandleCounterSurvivesResume`                 | `LoadSession` folds the record; next handle continues the series |
| `TestToolResultHandleMetadataSurvivesResume`                | a resumed session can read a previous process's handle           |
| `TestLoadSessionSkipsMalformedRetainedRecord`               | malformed/duplicate handles are skipped, not folded              |
| `TestReadToolResultRangeRead`                               | `offset`/`limit` line window                                     |
| `TestReadToolResultDefaultsToHead`                          | no args reads from line 1                                        |
| `TestReadToolResultLiteralSearch`                           | `search` returns numbered matching lines                         |
| `TestReadToolResultSearchIsLiteralNotRegex`                 | a regex metacharacter matches literally                          |
| `TestReadToolResultBoundedByMaxBytes`                       | `max_bytes` binds and says so                                    |
| `TestReadToolResultLimitHardCapped`                         | an absurd `limit` is clamped                                     |
| `TestReadToolResultUnknownHandle`                           | clean error naming known handles                                 |
| `TestReadToolResultMissingFile`                             | clean error, no raw path leak                                    |
| `TestReadToolResultToolRegisteredOnlyWhenEnabled`           | tool presence tracks the config gate                             |
| `TestProviderReceivesPreviewNotFullText`                    | **fake provider**: request 2 carries the preview, not the bytes  |
| `TestToolResultRetentionTouchesNoTranscoder` (repo-level)   | asserted by review + §7; no provider/ or wire code in the diff   |
| `TestReadToolResultOutputIsNeverRetained` (F2)               | read_tool_result's own output is exempt from retention           |
| `TestReadToolResultSurvivesOversizedLine` (F1)               | a line ≥ the scan buffer falls back to a raw byte read            |
| `TestReadToolResultRejectsMaxBytesBelowFloor` (F11)          | `max_bytes` under the floor is a clear error, not "no lines"     |
| `TestReadToolResultMaxMaxBytesIsPinned` (F6b)                 | the documented 65536 hard cap, pinned by value                    |
| `TestToolResultRetainedFileIsMaskedAndPrivate` (F4)          | secret-shaped lines are masked on disk; file/dir modes are private|
| `TestMaskSecretsPreservesBenignText` (F4)                    | the masker does not corrupt ordinary prose                        |
| `TestLoadSessionRegistersReadToolResultForExistingHandles` (F12) | a resumed, retention-disabled session can still read old handles |
| `TestCompactPreservesRetainedResultsIndex` (F3a)             | compaction carries a deterministic handle index forward           |
| `TestCompactRetainedResultsIndexOmittedWhenNoHandles` (F3a)  | no spurious index block when nothing is retained                  |
| `TestParseToolResultHandle` (F13)                            | digits-only handle grammar; `trh_+1`/`trh_01` rejected             |

The fake-provider test is the load-bearing one: it drives a real
`Session.Prompt` with a scripted provider whose first turn calls a tool
producing far more than the inline limit, then asserts against the *second*
request the provider received that the serialized tool result contains the
handle and does **not** contain the original bytes.
