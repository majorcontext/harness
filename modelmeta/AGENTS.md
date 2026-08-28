# Model metadata instructions

These rules apply to `modelmeta/`. Harness does not merge ancestor files. If
root guidance is not active, locate the Git root and read
`<repo-root>/AGENTS.md`. Resolve repository paths from that root. Read
`engine/AGENTS.md` before changing context-window policy.

## Static metadata

Keep model metadata static and deterministic. This package must not perform a
network request or refresh in the background.

Curate context-window values from the documented source. Keep zero unavailable
for non-chat models because zero also means unknown to callers.

## Lookup

Keep provider-family matching explicit. Preserve documented handling for dated
model variants and aliases. An unknown model returns no window; the engine owns
the refusal or opt-out policy.

Do not add capability guesses from model-name substrings unless a design and
tests define the contract.

## Server-side tool search

`SupportsToolSearch` uses an explicit first-party Anthropic allowlist. Return
false for other provider families and for Bedrock-style Anthropic refs. An
unknown ref must keep the portable client-side search path instead of emitting
a provider tool that the route can reject.

Keep its Bifrost namespace stripping aligned with context-window lookup.

## Tests

Test exact known refs, supported variant patterns, near misses, unknown
families, and tool-search refusals. A metadata update must include the source
and lookup regression tests.
