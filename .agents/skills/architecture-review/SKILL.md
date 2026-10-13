---
name: architecture-review
description: Judge a change against the architecture as a whole, not the diff alone. Use on every review of a pull request alongside the correctness review. Checks spec divergence, contracts, error and write ownership, parity, structure, tests, docs, and proportion.
---

# Architecture review

A diff can be correct line by line and still make the system worse: a second copy of a fact, a rule in the wrong layer, a patch over a symptom of a boundary that is wrong. Read the change, read the spec sections it touches, and judge the change against the whole. This adds to the correctness review; it does not replace it.

Read the repo's AGENTS.md for its gates, layering rules, and spec location. This skill points; the spec decides. If the spec is not in the checkout, say so and do not guess its content.

## Questions

A "yes" is a finding. Cite the spec as `path:line` and the code as `file:line`. A finding without a citation is an opinion; label it as one.

1. **Spec.** Does the change differ from the spec? That is a blocker unless the same PR updates the spec and records a dated decision. A rationale in the PR body alone does not count.
2. **Contracts and wire.** Do wire types, routes, and clients come from the one contract source? Is a type hand-written that is already generated? Is generated output regenerated in the same PR? Does a fake, stub, or golden differ from the real component in shape, status, error body, or order?
3. **Error ownership.** Is there one error envelope and one code table? Are errors passed through unchanged, not re-shaped or matched by string?
4. **Single writer.** Does each record, table, field, and route have one owner and one writer? Does the change add a second record, resolver, constant set, or implementation of a concept that has an owner? Search the tree for the existing one and name the owner.
5. **Workaround on a workaround.** Does the change add a retry, poll, cache, flag, or special case over a boundary problem? Name the boundary that should move.
6. **Parity.** When a system is replaced, does the new one keep the old behavior? A deliberate break needs a recorded decision. New code must not read or call anything the spec deletes.
7. **Structure.**
   - Layering: does an import cross a gate, or point from a lower layer up to a higher one? Does the change widen an import rule without a spec line? Is policy in the transport, or does code branch on a provider or backend name? Is the interface declared in its consumer?
   - Duplicate types: is a type or concept defined again in another package?
   - Package purpose and size: does the package keep one purpose? Does a file pass about 1,000 lines or a function about 150 without a stated reason (unless the repo sets other limits)?
   - Logic in `cmd/`: is policy in a main package instead of a testable one?
   - Grab-bag names: packages `util`, `common`, `misc`, `helpers`, `api`, `types`; files `types.go`, `interfaces.go`.
   - Go API rules: apply the `go-guide` skill to every new or changed exported API.
8. **Tests.**
   - Each deleted test must cover only deleted code; a test deleted with live behavior still behind it is a regression.
   - Behavior is pinned by a contract test through the public entry point, not by tests of internals.
   - No sleeps; no history in tests or comments.
   - Does a new test pin a behavior that an existing contract test or another new test already pins, at the same level and with the same cases? A fix extends the test that covers the code; it does not add a sibling. A unit test of pure code that checks cases the contract test does not is not a duplicate.
   - Were tests moved, merged, split, or listed as exceptions only so that a test gate passes? That is a finding about the tests, not the gate: name the tests to delete or rewrite.
9. **Docs.** Docs ship with the code: a change to behavior, a package, or a route updates the spec or README in the same PR. A design-only PR is a finding.
10. **Proportion.** Judge size on the lines a reviewer reads: leave out generated output such as goldens and lines that only move.
    - Scope: does the PR hold more than one concern? Count the changes that could merge or revert alone. More than one is a finding unless a recorded decision makes them one unit; an approval of each change is not an approval to ship them together. Name the split.
    - Size: is a part larger than its problem needs? Name the code, test, or doc that can go with no loss of behavior or coverage.
    - Comments: does a comment that no repo rule requires only restate the name, the type, or the code next to it? A doc comment a repo rule requires is not a finding when it is one line. When a changed file has a comment-share warning, read each of its comments against this question.

## Reporting

- Lead with architecture findings, then correctness findings.
- Verify before you report: open the file, read the cited lines, and confirm the claim. Do not report from memory or from the diff alone.
- State each finding as the question number, the spec line, the `file:line`, and the smallest change that restores the boundary.
- Block on scope, unrecorded spec divergence, regression, a second source of truth, a read of a deleted surface, or tests reshaped only to pass a gate; block on a wrong layer only when it would be hard to move later. Mark the rest as non-blocking with their cost if left.
- A correct finding is not a requirement; weigh its cost against its benefit.
- Ignore pre-existing violations the change does not touch or worsen. Say so when the change touches one and leaves it as it was.
- Do not restate the spec or list what held. When there is nothing, say "no architecture findings" and name the spec sections read.
- Do not flag style.
