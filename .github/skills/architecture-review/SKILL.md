---
name: architecture-review
description: Judge a pull request against the architecture as a whole, not the diff alone. Use on every review of harness code, docs, or CI config. Checks single sources of truth, layering, deleted surfaces, workarounds, unrecorded spec divergence, test doubles, and generated types.
---

# Architecture review

A diff can be correct and still be wrong for the system. Review the change against the architecture as a whole. This skill adds to the correctness review; it does not replace it.

## Where the architecture is written

Read these before you judge. Do not rely on memory or on this file. This file points; the spec decides.

- `docs/architecture.md` in this repo is the binding spec. Sections: `Goals and non-goals`, `Four rules`, `Architecture` (`Public packages`, `Internal packages`), `HTTP` (`Routes`, `Errors`, `Contract source`), `turn and backend` (`Backend`), `Feature disposition`, `Tests and guardrails` (`Contract suite`, `Unit tests`, `CI gates`), `Boxes integration`, `Deliberate parity breaks`, `Decided`.
- The boxes spec is `docs/rearchitecture.md` in `meetneptune/boxes` (on `main`; the release branch `cutover/harness-switch` while it exists). Read it when the change touches a seam with boxes: `protocol`, `Sync`, `Store`, `Owner`, routes that boxes forwards, or `config` keys that boxinit writes.
- The root `AGENTS.md` and each scoped `AGENTS.md` that governs a changed path.

If the spec file is not in your checkout, say so in the review. Do not guess its content.

## Questions to answer for every change

Answer each one with a spec line, written as `docs/architecture.md:<line>`. A finding without a cited line is an opinion. State the question, the line, and the code that breaks it.

1. Second source of truth. Does the change add a second owner of state, or a second implementation of a concept that already has one (a resolver, a fold of the log, a status vocabulary, a writer)? Search the tree for the existing one before you accept the new one.
2. Wrong layer. Is policy in the transport (`internal/server`), a backend branching on a provider name, or session logic outside the actor? Check `Four rules` and `Internal packages` for who owns the concern.
3. Deleted surface. Does the change read, call, route to, or re-create something that the spec deletes (a route, a package, a config key, a record kind)? `Routes`, `Internal packages`, `Feature disposition`, and `Decided` list them.
4. Workaround on a workaround. Does the change patch a symptom of a boundary problem (a retry, a fallback, a special case, a repair site) instead of fixing the boundary? Name the boundary and the owner that should change.
5. Unrecorded divergence. Does the change differ from the spec? A divergence is a blocker unless the same PR updates the spec and records a dated Andy decision in the spec text, in `Decided`, or in `Deliberate parity breaks`. A PR body that cites no spec lines is a finding.
6. Test double against the real wire. Does a changed double (`harnesstest`, `harnesstest/fakeclaude`, a golden, a fixture) match what the real component it imitates sends and accepts (the provider API, the Claude Code stream-json, `harness serve`)? A double that drifts from the real wire is a finding. For where tests belong, read `Contract suite`, `Unit tests`, and the `CI gates` row on test lines outside the contract suite.
7. Duplicated type. Does a hand-written type repeat a `protocol` type or a generated one (`protocol/openapi.json`, `protocol/protocol.ts`)? Generated output must come from `go generate ./protocol` in the same PR.

## Invariants the review checks

These are pointers. Open the section named, read the current text, and check the change against it. Never copy the rule text into a review or into this file.

| Invariant | Where it is written |
| --- | --- |
| One append-only log, one `Apply` | `Four rules`, `eventlog` |
| One owner and one writer per piece of state | `Goals and non-goals`, `Four rules` |
| One goroutine owns a session; others read views | `Four rules`, `session` `Actor` |
| Interface lives in its consumer; no backend or provider name branches | `Four rules`, `Backend` |
| One resolver per concept (the context window is `Capabilities(model)` of the backend and nothing else) | `Backend`, `Model API backend` |
| One error envelope and its code table | `Errors` |
| One route table, mounted routes equal the spec's routes | `Routes`, `Contract source` |
| Public API is the packages that `Public packages` lists; the rest is `internal/` | `Public packages`, `Internal packages` |
| Import graph frozen by `depguard` | `Internal packages`, `.golangci.yml` |
| Wire types are generated from `protocol` | `Contract source` |
| Contract tests only; no `time.Sleep`; `synctest` for time | `Tests and guardrails`, `CI gates` |
| No history in comments or tests | `CI gates`, root `AGENTS.md` |
| Routes that boxes forwards stay 1:1 with the harness route | `Boxes integration`, boxes `docs/rearchitecture.md` |

## How to report

- Lead with the architecture findings, then the correctness findings.
- Rank a spec divergence, a second source of truth, and a deleted surface as blockers. Rank a layering concern as a blocker when the wrong layer would be hard to move later.
- Give each finding: the question number, the spec line, the code line, and the smallest change that restores the boundary.
- Say plainly when the change passes: "Checked questions 1 to 7 against the spec; no finding." Do not invent a finding to fill space.
- Do not flag a pre-existing violation that the change does not touch or worsen.
- Do not flag style. A correct finding is not a requirement; judge its cost against its benefit.
