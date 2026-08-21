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

A retention that is **refused** because retaining it would push the
per-session retained-bytes total over the ceiling emits a different, equally
fixed line (`engine.toolResultCapHeader`) and no handle, since there is
nothing to read back:

```
[tool result truncated: tool=bash bytes=123456 preview_bytes=16384 — retaining this result would exceed the per-session retention budget; its remainder is discarded irrecoverably, though a smaller result later this session may still be retained]
```

**Round-3 correction.** An earlier version of this wording (review finding
F3(b)) said "the per-session retention cap has been reached ... no further
tool result will be retained this session" — deliberately blunt, on the
theory that the ceiling is monotonic so this could only get worse. A later
review round caught that claim as false in both directions: `toolResultBytes`
is **not** incremented on a refusal (only a successful `writeRetainedToolResult`
increments it), so (a) the FIRST oversized result on a fresh session
(`used=0`) refuses unconditionally too, which is not "the cap has been
reached" by any accumulation, and (b) a LATER, SMALLER result can still fit
under the same ceiling and succeed — directly contradicting "no further tool
result will be retained." `TestToolResultCapHeaderDoesNotOverstatePermanence`
drives that exact contradiction end to end. The wording now says only what is
always true: retaining THIS result would exceed the remaining budget, and
THIS result's remainder is gone for good — with no claim about what happens
next.

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
retention entirely. **The notice is now counted INSIDE the budget** (review
finding N10, round 2): the trailing continuation/truncation notice used to be
appended AFTER the body was budgeted against the full `max_bytes`, so
body+notice together could exceed `max_bytes` by however long the notice text
was (measured ~50-80 bytes). `readToolResultNoticeReserve` (128 bytes,
generous) is now carved out of the budget BEFORE the per-line/per-match loop
runs, in both modes, so the two together never exceed the caller's
`max_bytes`.

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
now checks `sc.Err()` and, on `ErrTooLong`, retries once against
`readerAtLineSource`, a raw `io.ReaderAt`-based line source with no per-line
limit — `ReaderAt` specifically because it is an absolute-offset read,
independent of whatever the failed `bufio.Scanner` already consumed from the
same `*os.File`'s read cursor.

Two round-2 fixes to this fallback:

- **Search mode was still half-broken (review finding N1).** The rescue
  above was originally range-mode only; `readToolResultSearch` still dropped
  a too-large matching entry WHOLE, reporting "0 match(es)" — a false
  negative on exactly the retained-result-is-one-enormous-line case F1 is
  about, with unusable advice ("narrow the search" on a file that IS one
  line). Search mode now emits a truncated **window around the match**
  (`extractMatchWindow`) rather than a from-byte-0 prefix: the match can sit
  megabytes into a multi-megabyte line, well past what a prefix window would
  ever reach, so the window is anchored a small fixed distance BEFORE the
  match's byte offset instead. The regression test that first covered this
  (`TestReadToolResultSurvivesOversizedLine`) was itself tautological — its
  whole-output assertion passed even with matching completely broken,
  because the header line echoes the search needle back via `lines matching
  %q:` regardless of whether anything matched. The assertion now checks only
  the body AFTER the header line;
  `TestReadToolResultSearchNeverMatchMutant` forces matching off via a test
  hook and confirms that split actually catches it.
- **The fallback buffered the whole file (review finding N7).** The first
  cut allocated `make([]byte, meta.Bytes)` and read the ENTIRE file in one
  `ReadAt` call, even to satisfy a 256-byte range request — defeating the
  memory-bounded design retention exists for. `readerAtLineSource` now
  streams in fixed 64 KiB chunks, growing its buffer only as far as the
  caller actually scans; once a range/search loop breaks (limit reached,
  budget hit, match found), nothing past that point is ever read from disk.

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

**Round 3: four more fixes.**

- **`readerAtLineSource` could busy-loop forever (CRITICAL).** If the real
  sidecar file is SHORTER than `meta.Bytes` claims — a volume rollback, an
  operator's partial wipe of `toolresults/`, exactly the mismatch class the
  missing-file handling above already anticipates — `ReadAt` at true EOF
  returns `(0, io.EOF)`. `l.off` never advanced (n==0), `io.EOF` was
  excluded from the error check on purpose (to tolerate an ordinary short
  final read), so nothing ever set `l.err` OR advanced `l.off` to the
  (inflated) claimed size: the loop spun `IndexByte → ReadAt → (0, io.EOF)`
  forever, 100% CPU, wedging the run slot indefinitely. Fixed: a zero-byte
  `io.EOF` read is now treated as the true end of data — `l.off` is pinned
  to `l.size` so the existing size-reached branch fires on the next
  iteration. `TestReaderAtLineSourceTerminatesWhenFileShorterThanClaimedSize`
  proves termination with a goroutine-plus-timeout watchdog (the only way to
  observe "never returns" without hanging the test run itself).
- **Every `os.Open` error was reported as "no longer on disk".**
  `openRetainedToolResult` mapped ANY error — not just `os.ErrNotExist` — to
  the terminal "gone" wording, so a transient permission or I/O error got
  the same steer-away-from-retrying message a genuinely deleted file gets.
  Fixed: only a true not-exist gets that wording now; anything else returns
  the raw error, which the caller's existing generic "cannot read handle"
  fallback already handles without leaking an absolute path.
- **A byte-truncated PARTIAL first line made its own remainder permanently
  unreachable.** When the very first shown line alone exceeds `max_bytes`,
  only a truncated prefix is emitted — but the notice reported "continue
  with `offset=offset+1`", silently skipping past whatever this SAME line
  didn't fit. Every future read at that name would start from the next
  line too, so the unshown remainder could never be retrieved at ANY
  `max_bytes`. Fixed: this specific case now keeps the continuation
  `offset` UNCHANGED and says so explicitly ("increase max_bytes and
  re-read at the same offset=N") — a caller that does this re-scans from
  byte zero and genuinely reaches further into the same line, which is
  real, verified progress, not just a more honest dead end.
- **The match-count cap's notice wrongly suggested raising `max_bytes`.**
  Hitting `readToolResultMaxLimit` (2000 matches) set the same flag a
  byte-budget stop uses, so the notice always said "...narrow the search or
  increase max_bytes" — but raising `max_bytes` cannot surface a single
  additional match once counting stopped; the search simply stopped
  counting, not running out of byte budget. A separate `countCapped` flag
  now selects a distinct notice ("stopped at N match(es) (the match-count
  limit); narrow the search — increasing max_bytes will not surface
  additional matches").

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

**The index is now bounded (review finding N8, round 2).** The first cut
listed EVERY live handle, unconditionally — with the ceiling disabled
(`Config.ToolResultRetainedBytes <= 0`) a long session can mint arbitrarily
many, and 200 handles measured at roughly 6.9k tokens of index text, which
defeats the point of compaction (reducing what the next request pays for).
`retainedResultsIndexMaxHandles` (32) caps the list to the NEWEST handles —
newest, because those are the ones most likely to still matter to the
conversation about to continue — with a trailing `...and N older retained
result(s)` count-only line for the rest.

**The index no longer claims readability it hasn't checked (review finding
N9).** Each listed handle's sidecar file is `os.Stat`'d before being
described as "still readable via read_tool_result"; a handle whose file is
gone (an operator wiped `toolresults/`, a volume rolled back) is still
listed — its metadata is real and it still counts against the ceiling — but
annotated "sidecar file missing, no longer readable" instead.

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

1. **File permissions.** The sidecar directory and file are `0o700`/`0o600`
   (were `0o755`/`0o644`) — private to the process owner, not group- or
   world-readable.
2. **Masking.** `engine/toolresult_secrets.go`'s `maskSecrets` is a
   best-effort, pattern-based redaction. This repo had no existing
   secret-masking utility to reuse.

**Round 2 rewrite (findings N2-N6, N11).** F4's first cut used one pattern
with an unbounded `\S+` value half. A second review round found this
actively destructive, not just incomplete:

- **N2 (data loss).** `\S+` is greedy to the next WHITESPACE, not to the
  next structural delimiter — a single incidental match (`&token=` inside a
  URL, `token=<huge blob>` on one line with no other whitespace) deleted
  everything from the match to the next whitespace. Measured 2,097,164 bytes
  of a 4 MiB single-line retained result destroyed by one masked "value"
  that was mostly unrelated adjacent content. The value class is now bounded
  on two independent axes: a character class (`[A-Za-z0-9_\-./+=]`) that
  excludes exactly the delimiters that matter (`&`, `?`, `,`, `"`, `}`,
  whitespace, code punctuation), so a match naturally stops at the next real
  delimiter, AND a length cap (`{8,200}`, `{8,1000}` for a Bearer token) as
  a second bound. `meta.Bytes`/`Lines`/`Head` are now measured from the
  MASKED, on-disk text, not the pre-mask original — the first cut reported
  the original length, so a header or `read_tool_result` could advertise a
  size the sidecar file did not actually have.
- **N3 (missed shapes).** Added quoted-JSON (`"token": "..."`,
  `"api_key":"..."`) and `Authorization: Bearer <token>`, alongside the
  existing `KEY=value`/`KEY: value` (space-YAML) shape.
- **N4 (code corruption).** `token:=lexer.Next()` (Go's `:=` short variable
  declaration) became `token:*** if...` — the old pattern treated the bare
  `:` ahead of `=lexer.Next()` as a key/value separator. The colon
  alternative now requires MANDATORY whitespace after it (`:[ \t]+`), never
  bare `:` — which is what excludes `:=` structurally: nothing in the
  pattern can start a match where a Go short declaration's `:` is
  immediately followed by `=` with no space.
- **N5 (preview unmasked).** The PREVIEW half of a retained result — the
  bytes that go straight into the provider request, inline — was built from
  the unmasked original; only what reached disk was masked. A secret
  sitting within the first `ToolResultInlineBytes` reached the model in
  cleartext regardless of masking existing at all. `maybeRetainToolResult`
  now masks exactly ONCE, up front, and derives the preview, the on-disk
  bytes, `meta.Bytes`/`Lines`/`Head`, and the retention-ceiling accounting
  all from that one masked value.
- **N6 (performance).** Three sequential `ReplaceAllString` passes (one per
  shape) measured SLOWER (556-635ms/4.4MB) than the original single
  unbounded pattern (352ms/4.4MB) — each pass re-scans the whole string
  independently. Two changes: (a) the three shapes are now one combined
  regexp, walked once via `FindAllStringSubmatchIndex` and rebuilt in a
  single `strings.Builder` pass; (b) `containsSecretCandidate`, a cheap
  non-regex substring pre-filter, skips the (expensive — RE2's automaton for
  this pattern, with its unrolled bounded repeats, is measurably slow even
  on a NO-MATCH scan) regex entirely for text with no candidate keyword at
  all, applied both to the whole input and per-line (none of the three
  shapes ever spans a newline, so line-level filtering is exactly
  equivalent to whole-text — a line with no candidate keyword is copied
  verbatim at `Contains` cost, never touching the regex engine). Measured on
  this branch: ~20-25ms/4.4MB for ordinary multi-line output (with or
  without scattered secrets) — comfortably under the 100ms/4MB target, and
  a large improvement over the 352ms original baseline. The one case that
  does NOT benefit from the line-level filter is the F1 pathological
  single-huge-line-with-no-newlines input; that falls back to one full-text
  regex scan and measured ~650ms/4.4MB — a documented, accepted residual
  for a rare edge case, not a regression target.

**Round 3: quoted env/YAML values.** `export TOKEN="secretvalue123"` — an
UNQUOTED key with a QUOTED value, an extremely common shell/env-dump shape —
slipped through entirely unmasked: the env/YAML alternative required its
value class immediately after the separator (the next byte there, `"`, is
not in `secretValueClass`), and the JSON alternative requires a QUOTED key,
which a bare `TOKEN` lacks. Two more alternatives cover double- and
single-quoted values now (spelled out separately — RE2 has no
backreferences, so "whichever quote opened" cannot be one pattern).
`TestMaskSecretsQuotedEnvValue` covers both quote styles plus quoted-YAML.
This grew the combined pattern by two alternatives, which measurably slowed
the F1 pathological single-huge-line case (§6) further — see the updated
number in the test list below and the PR body.

**Residual risk, explicitly not fully solved.** This is a minimal pattern
matcher, not a secret scanner: it only catches the shapes listed above, only
for a fixed key-name list, and has no way to catch a bare token pasted with
no label at all, or a secret value nested under an unrecognized key name. A
real fix (a pluggable/configurable masking policy, or integration with a
proper secret-detection library) is follow-up work, not this PR.

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
| `TestReadToolResultSurvivesOversizedLine` (F1/N1)             | oversized-line range AND search both recover; body-only assertion  |
| `TestReadToolResultSearchNeverMatchMutant` (N1)               | mutation-verification: a forced never-match is caught, not tautological |
| `TestReaderAtLineSourceStreamsRatherThanBuffersWholeFile` (N7)| the fallback streams in chunks, not one whole-file read            |
| `TestReaderAtLineSourceReadsWholeContentWhenScannedToCompletion` (N7) | streaming correctness when fully scanned                   |
| `TestReadToolResultOutputNeverExceedsMaxBytes` (N10)          | body+notice together never exceed `max_bytes`, both modes          |
| `TestMaskSecretsDoesNotDeleteAdjacentContent` (N2)            | a bounded value match never deletes adjacent unrelated data        |
| `TestMaskSecretsQuotedJSON` (N3)                              | `"key": "value"` and `"key":"value"`, incl. compound keys           |
| `TestMaskSecretsSpaceYAML` (N3)                               | `key: value` (space-YAML) masked, surrounding YAML untouched        |
| `TestMaskSecretsAuthorizationBearer` (N3)                     | `Authorization: Bearer <token>` masked                              |
| `TestMaskSecretsCodeCorpus` (N4)                              | realistic Go/Python/JS/TS snippets survive byte-identical, incl. `:=` |
| `TestMaskSecretsPreview` (N5)                                 | the inline preview is masked, not just the sidecar file             |
| `TestToolResultMetaBytesMatchesOnDiskLength` (N2)             | `meta.Bytes` equals the actual on-disk (post-mask) file length      |
| `TestMaskSecretsPerformance` (N6)                             | three input shapes measured against N6's targets; see PR body       |
| `TestRetainedResultsIndexCapped` (N8)                         | the compaction index lists only the newest 32, names the rest by count |
| `TestRetainedResultsIndexNotesMissingSidecar` (N9)            | a handle with no sidecar file is annotated, not claimed readable    |
| `TestReaderAtLineSourceTerminatesWhenFileShorterThanClaimedSize` (round 3, critical) | no busy-loop when the sidecar is shorter than meta.Bytes |
| `TestReadToolResultNonMissingErrorIsNotReportedAsGone` (round 3) | only a true not-exist gets the terminal "gone" wording          |
| `TestToolResultCapHeaderDoesNotOverstatePermanence` (round 3) | the cap header never claims permanence the ceiling doesn't have     |
| `TestReadToolResultPartialFirstLineOffsetIsRecoverable` (round 3) | a byte-truncated first line's remainder is genuinely reachable  |
| `TestReadToolResultSearchCountCapNoticeDoesNotSuggestMaxBytes` (round 3) | count-cap notice doesn't suggest raising max_bytes        |
| `TestMaskSecretsQuotedEnvValue` (round 3)                     | `KEY="value"`/`KEY='value'`/quoted-YAML masked                       |

The fake-provider test is the load-bearing one: it drives a real
`Session.Prompt` with a scripted provider whose first turn calls a tool
producing far more than the inline limit, then asserts against the *second*
request the provider received that the serialized tool result contains the
handle and does **not** contain the original bytes.
