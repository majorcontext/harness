# Nested, on-demand instruction loading

Status: design, for review. No implementation yet.

## Problem

`engine/instructions.go` injects one project instruction file into the system
prompt and caps it (`InstructionsConfig.MaxBytes`, 64 KiB by default). The cap
is now loud on both channels: the model reads a marker, the operator reads a
WARN line. Loud is not enough. The content past the cap is still absent from
the prompt, and the model must guess that it needs the rest.

Two files make this concrete. The boxes repository has a 408 KiB `AGENTS.md`:
84% of it never reaches the model. This repository's own `AGENTS.md` is
180,631 bytes over 2,866 lines with 51 headings: 116 KiB of binding
specification is dropped from every session today.

## What the model sees

The instruction segment gets two parts instead of one.

**The head** is the file up to the last complete section boundary at or under
`MaxBytes`. It is the same eager, always-present text the segment carries
today, cut on a heading rather than on an arbitrary byte, so a section is
never shown half.

**The outline** replaces the dropped tail. One line per section that the head
does not fully contain, each line carrying the heading text, the exact
`read_file` line range, and a short teaser from the section body:

```
Project instructions from AGENTS.md (sections 17-51 are not in this prompt).
Read a section with the read_file tool before you rely on it:
  read_file(path: /repo/AGENTS.md, offset: <first>, limit: <count>)

  Goal loop — lines 659-1041 — Session.PursueGoal drives the ordinary Prompt
  loop toward a natural-language completion condition...
  Session metadata index — lines 1042-1104 — ...
```

This is the engine's existing Agent Skills stage-1/stage-2 split, applied to
one file instead of a directory of them: an index the model must read through
before it relies on a section. The wording is the skills wording ("you MUST
read ... before relying on it"), so the model treats both the same way.

## The mechanism it uses to pull a section

`read_file`, with no new tool. Its `offset` and `limit` are already 1-based
line numbers (`engine/filetools.go`), the outline supplies exact ranges, and
the read costs one ordinary tool call that the tool-read budget and the
read-set bookkeeping already govern. A dedicated `instructions` tool with
`outline`/`section` actions was considered and rejected: it adds a schema to
every request for a capability `read_file` already has, and skills stage 2
already reuses `read_file` for exactly this.

Reference markers the model "expands" were also rejected. An expansion marker
needs a protocol the model can invoke, which is a tool by another name, and it
would put a second retrieval path next to `read_file` for the same bytes.

## Nested files, adapted from opencode `resolve()`

opencode's `instruction.ts` `resolve()` (~179-221) is the second half: when
the model reads a file, it walks up from that file's directory to the project
root, finds an `AGENTS.md` that is neither in the system prompt nor already
attached, and appends it to that read's result — deduped per message through a
claims map and a scan of earlier completed read parts. fx scopes instruction
loading the same way: instructions arrive when the work enters their scope.

The harness port fits the tool-result path, not the system prompt.
`read_file` (and `edit_file`/`write_file`) resolves a path through
`s.resolvePath`; from that resolved path the engine walks up to
`Config.WorkDir`, stopping at the git root exactly as `loadInstructions` does,
and appends a `message.Text` part per newly found nested instruction file to
that tool result. A per-session attached-set — runtime only, never persisted,
the shape `Session.recordRead` already uses in `engine/filetools.go` — makes
each nested file arrive at most once per session. The root file the system
prompt already carries is always skipped. Nested files pass through the same
`truncateInstructions` cap, so a large nested file is still loud.

## How this composes with the truncation cap

They stack in one direction: the cap decides what is EAGER, the outline makes
everything else REACHABLE. Nothing is silently dropped in either mode — the
invariant the loud-truncation change established holds unchanged.

- File at or under the cap: byte-identical to today. No outline, no marker.
- File over the cap with usable headings: head + outline. The outline carries
  the loud notice, so it REPLACES the truncation marker for this file. The
  WARN log line stays, with the section counts added.
- File over the cap with no usable headings (one giant section, a generated
  file): the head + marker path from the loud-truncation change, unchanged.
  This fallback is why that marker stays in the code.
- `MaxBytes` negative (cap disabled): the whole file is injected, no outline.

The outline has its own budget so a pathological file cannot spend the prompt
on an index: teasers are dropped first, then the list degrades to headings and
ranges only. Measured on this repository's `AGENTS.md`: 35 outlined sections
cost 5,965 bytes with teasers, 1,424 bytes without. A 408 KiB file with the
same heading density holds roughly 115 sections, so its outline lands near
19 KiB with teasers and near 4.7 KiB without — the budget picks the second.

## The 408 KiB boxes AGENTS.md, concretely

Eager: the head, the first sections up to the last heading boundary under
64 KiB. Measured on this repository's file, that boundary is byte 41,648 —
sections 1-16 of 51. Cutting on a heading costs 23 KiB of eager text against
a raw byte cut, and buys a head that never ends mid-sentence; the sections
that pay that cost are listed in the outline, so they are reachable, not lost.

On demand: sections 17-51, each one `read_file` call away, with the exact
range in the prompt. Today those 116 KiB are unreachable. For the boxes file
the same split makes 344 KiB reachable instead of dropped.

## Configuration

`InstructionsConfig.Mode`: `auto` (the default — outline when the file is over
the cap and has usable headings), `full` (the pre-outline behavior: head plus
the truncation marker). Config key `instructions_mode`, operator seam
`HARNESS_INSTRUCTIONS_MODE`, resolved in `cmd/harness` like
`HARNESS_INSTRUCTIONS_MAX_KB`. Nested attachment gets its own switch,
`instructions_nested` (default on), because it changes tool results rather
than the system prompt.

## What this design does not do

It does not follow Markdown links to other documents. A section that names
`docs/design/context-compaction.md` gives the model a path, and `read_file`
reads it; a link crawler would pull unbounded content nobody asked for.

It does not re-read the file per request. The segment and the outline are
built from the single read `ensureInstructions` already performs, cached for
the session, and never written to the session log.

## Risks the tests must pin

1. **Line-number accuracy.** An off-by-one makes every outline range wrong.
   The test drives the real `read_file` tool with the outline's own ranges and
   compares the returned lines against the file, rather than comparing the
   outline to itself.
2. **Fenced code blocks.** This repository's `AGENTS.md` holds ```` ```bash ````
   blocks whose comment lines start with `#`. A naive heading scan reads them
   as sections and emits ranges that point at shell comments. The scanner
   tracks fences; a test file with a `#` line inside a fence pins it.
3. **Head boundary.** The head must end on a heading boundary and must never
   exceed `MaxBytes`. A file whose first heading is past the cap has no
   boundary to cut on and falls back to the marker path.
4. **Every byte is accounted for.** Head lines plus outlined ranges must cover
   the whole file with no gap and no overlap. This is one property test over
   generated files, and it is the strongest guard the design has.
5. **Nested attachment fires once.** A second read under the same directory
   attaches nothing; a reload starts with an empty attached set.

## Proposed split

PR 2 is the head, the outline, and the mode switch — the system-prompt side
that the 408 KiB file needs. PR 3 is the nested attach-on-read port of
`resolve()`. They touch different paths (system prompt versus tool result),
carry different failure modes, and each one is small enough to review to zero.
Landing them together would put a prompt-assembly change and a tool-result
change under one review.
