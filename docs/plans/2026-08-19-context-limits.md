# Context limits table (fx/context-limits)

Status: in progress.

## Goal

Consolidate harness's scattered hardcoded byte budgets into one named,
configurable table.

## Config surface

New JSON key `context_limits` (package config): an object mapping known
limit names to either a non-negative integer (bytes) or the literal string
`"off"`.

Keys (v1, exactly three):

- `project_instructions_bytes` (default 65536) — the AGENTS.md truncation
  cap.
- `skill_catalog_bytes` (default 65536) — cap on the rendered stage-1
  skills system segment.
- `read_file_image_bytes` (default 20971520) — the `read_file` image read
  cap.

## Semantics

- `"off"` disables the named limit, but a hard emergency ceiling of
  67108864 (64 MiB) still applies at every wired site.
- An unknown key is a config load error naming the bad key and the valid
  keys.
- A negative number, non-integer number, or any other string is a config
  load error.

## Engine surface

`engine.ContextLimits` is a plain struct of resolved int fields (one per
limit), attached to `engine.Config`. Package engine never imports config;
config computes the resolved struct and passes it in, following the
`PromptRetriesValue`/`ModelToolEnabled` pattern. A zero-value
`engine.ContextLimits` resolves to the documented defaults via a
resolve()/getter approach — no init-time mutation.

## CLI surface

Repeatable flag `-context-limit key=value` on `run` and `serve`, following
the `-skills-dir` pattern. Overrides config. An unknown key or a malformed
value is a startup error.

## Wired sites

1. `engine/instructions.go` — AGENTS.md truncation (`maxInstructionsBytes`).
2. `engine/skills.go` — stage-1 skill catalog segment truncation (new).
3. `engine/filetools.go` — `read_file` image byte cap
   (`readFileMaxImageBytes`).

(This section will be filled in with final field/flag names as the
implementation lands.)
