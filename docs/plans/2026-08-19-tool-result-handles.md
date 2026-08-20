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

`bash` already caps one call's combined output at
`Config.BashOutputCap` (96 KiB, `engine/bash.go`). Two problems remain:

1. The cap is destructive and terminal. The bytes past it are gone; a
   `go test ./...` run whose one interesting FAIL line sits at byte 300k is
   simply unrecoverable, and the agent's only recourse is to re-run the
   command.
2. The cap does not apply to MCP tools or plugin tools at all, and 96 KiB is
   still large: harness re-sends the whole history every request (stateless
   transcoding), so a single 96 KiB result is re-billed as input on every
   later turn for the life of the session.

Retention fixes both. The bytes are kept on disk, out of the context window,
and are addressable.

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
[tool result truncated: tool=bash bytes=123456 preview_bytes=16384 — retention cap reached for this session, the remainder is discarded]
```

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
- `max_bytes` — output byte ceiling. Default 16384, hard-capped at 65536.

Output is always bounded twice: by the line budget and by `max_bytes`,
whichever binds first, with an explicit truncation notice when either does.
Bounding is the whole point — an unbounded read back into context would defeat
retention entirely.

Degradation, both clean and both a normal tool error (never a panic, never a
partial read):

- **Unknown handle** — the error names the handle and lists the handles this
  session actually has (bounded to the most recent 20).
- **Missing file** — the handle is known but its sidecar file is gone
  (an operator wiped the directory, a volume rolled back). The error says so
  and names the handle, rather than surfacing a raw `os.PathError` with an
  absolute filesystem path.

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

## 8. Test list

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

The fake-provider test is the load-bearing one: it drives a real
`Session.Prompt` with a scripted provider whose first turn calls a tool
producing far more than the inline limit, then asserts against the *second*
request the provider received that the serialized tool result contains the
handle and does **not** contain the original bytes.
