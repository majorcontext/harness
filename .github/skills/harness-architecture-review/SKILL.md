---
name: harness-architecture-review
description: Harness-specific checks for the shared architecture-review skill: the spec sections, layering, deleted surfaces, single owners, wire doubles, proportion, and structure gates. Use on every review of harness code, docs, or CI config.
---

# Harness architecture checks

Apply the installed shared skills first: `.agents/skills/architecture-review/SKILL.md` for the review method and questions, and `.agents/skills/go-guide/SKILL.md` for Go layout, API, and test rules. This file adds only what is specific to harness. This file points; the spec decides.

## Where the architecture is written

Read these before you judge. Do not rely on memory or on this file. This file points; the spec decides.

- `docs/architecture.md` in this repo is the binding spec. Sections: `Goals and non-goals`, `Four rules`, `Rules for changes`, `Architecture` (`Public packages`, `Internal packages`), `eventlog`, `session`, `HTTP` (`Routes`, `Errors`, `Contract source`), `turn and backend` (`Backend`), `tool, prompt, and config`, `Tests and guardrails` (`Contract suite`, `Unit tests`, `CI gates`), `Boxes integration`.
- The boxes spec is `docs/rearchitecture.md` in `meetneptune/boxes` (on `main`; the release branch `cutover/harness-switch` while it exists). Read it when the change touches a seam with boxes: `protocol`, `Sync`, `Store`, `Owner`, routes that boxes forwards, or `config` keys that boxinit writes.
- The root `AGENTS.md` and each scoped `AGENTS.md` that governs a changed path.

If the spec file is not in your checkout, say so in the review. Do not guess its content.

## Harness questions for every change

Answer each one with a spec section and the quoted sentence, written as `docs/architecture.md`, section name. A finding without a cited sentence is an opinion. State the question, the sentence, and the code that breaks it.

1. Second source of truth. Does the change add a second owner of state, or a second implementation of a concept that already has one (a resolver, a fold of the log, a status vocabulary, a writer)? Search the tree for the existing one before you accept the new one.
2. Wrong layer. Is policy in the transport (`internal/server`), a backend branching on a provider name, or session logic outside the actor? Check `Four rules` and `Internal packages` for who owns the concern.
3. Deleted surface. Does the change read, call, route to, or re-create something that the spec deletes (a route, a package, a config key, a record kind)? `Routes`, `Internal packages`, and `config` list what exists.
4. Workaround on a workaround. Does the change patch a symptom of a boundary problem (a retry, a fallback, a special case, a repair site) instead of fixing the boundary? Name the boundary and the owner that should change.
5. Unrecorded divergence. Does the change differ from the spec? A divergence is a blocker unless the same PR updates the spec, as a decision of Andy, so that it describes the system as it is. A PR body that cites no spec sections is a finding.
6. Test double against the real wire. Does a changed double (`harnesstest`, `harnesstest/fakeclaude`, a golden, a fixture) match what the real component it imitates sends and accepts (the provider API, the Claude Code stream-json, `harness serve`)? A double that drifts from the real wire is a finding. For where tests belong, read `Contract suite`, `Unit tests`, and the `CI gates` row on test lines outside the contract suite.
7. Duplicated type. Does a hand-written type repeat a `protocol` type or a generated one (`protocol/openapi.json`, `protocol/protocol.ts`)? Generated output must come from `go generate ./protocol` in the same PR.
8. Proportion. Answer the shared skill's `Proportion` question with the harness inputs. One concern per PR is a root `AGENTS.md` `Spec and review` rule. Leave `testdata/wire` recordings, `go generate ./protocol` output, and goldens out of the size count. A new line in `testdata/test-exceptions.txt` needs a reason that names a path no `e2e/` row can reach; a test listed there that repeats a row is a finding. The `internal/gates` `comment_share` warnings name the files whose comments to read.

## Invariants the review checks

These are pointers. Open the section named, read the current text, and check the change against it. Never copy the rule text into a review or into this file.

| Invariant | Where it is written |
| --- | --- |
| One append-only log, one `Apply` | `Four rules`, `eventlog` |
| One owner and one writer per piece of state | `Goals and non-goals`, `Four rules` |
| One goroutine owns a session; others read views | `Four rules`, `session` `Actor` |
| Interface lives in its consumer; no backend or provider name branches | `Four rules`, `Backend` |
| `Capabilities(model)` resolves the model window; an owned-context session gauge prefers its latest nonzero measured window | `Backend`, `Model API backend`, `Claude Code` |
| One error envelope and its code table | `Errors` |
| One route table, mounted routes equal the spec's routes | `Routes`, `Contract source` |
| Public API is the packages that `Public packages` lists; the rest is `internal/` | `Public packages`, `Internal packages` |
| Import graph frozen by `depguard` | `Internal packages`, `.golangci.yml` |
| Wire types are generated from `protocol` | `Contract source` |
| Tests live where `Contract suite` and `Unit tests` allow; no `time.Sleep`; `synctest` for time | `Tests and guardrails`, `CI gates` |
| No history in comments or tests | `CI gates`, root `AGENTS.md` |
| Routes that boxes forwards stay 1:1 with the harness route | `Boxes integration`, boxes `docs/rearchitecture.md` |

## Structure

Check the code layout too. A "yes" is a finding; cite the file and line. The Go rules are `Four rules` and its Go guidance table in `docs/architecture.md`, and the root `AGENTS.md` `Code` section.

- Imports. A new import follows `Internal packages` and passes the `depguard` rules in `.golangci.yml`. A change that widens a `depguard` rule needs a spec line.
- Second definition. A type or concept defined a second time is a finding. Each record has one owner; name it.
- Package purpose and size. A package has one purpose. `internal/gates` fails a changed file over 800 lines or a function over 80 lines (root `AGENTS.md` `Code`).
- Logic in `cmd/`. Logic in `cmd/` that is not flag parsing and wiring belongs in `internal/`.
- Grab-bag names. No `util`, `common`, or `helpers` packages; no files named `types.go` or `interfaces.go`.
- Go API design. Apply the `go-guide` skill; names do not stutter (`session.Actor`, not `session.SessionActor`).

## How to report

- Lead with the architecture findings, then the correctness findings.
- Rank a spec divergence, a second source of truth, and a deleted surface as blockers. Rank a layering concern as a blocker when the wrong layer would be hard to move later.
- Give each finding: the question number, the spec section, the code line, and the smallest change that restores the boundary.
- Say plainly when the change passes: "Checked questions 1 to 8 against the spec; no finding." Do not invent a finding to fill space.
- Do not flag a pre-existing violation that the change does not touch or worsen.
- Do not flag style. A correct finding is not a requirement; judge its cost against its benefit.
