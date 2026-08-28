# Image clamp instructions

These rules apply to `imageclamp/`. Harness does not merge ancestor files. If
root guidance is not active, locate the Git root and read
`<repo-root>/AGENTS.md`. Resolve repository paths from that root. Read
`provider/AGENTS.md` before changing adapter limits.

## Transcode-time normalization

`Clamp` repairs a throwaway request. It must not mutate canonical history.
Return the original slice without allocation when no image changes.

Keep output deterministic so repeated transcodes remain prompt-cache stable.

## Bounds

Enforce both dimension and encoded-byte limits. Reject absurd dimensions or
pixel counts before a full decode. Use the text placeholder when safe decode or
useful downscale is impossible.

Do not remove the decode-memory guards. Do not rewrite the durable source blob.

The caller decides whether to recurse into tool results. Preserve adapter
differences documented in `provider/AGENTS.md`.

## Tests

Use small generated fixtures when practical. Cover copy-on-write behavior,
deterministic bytes, dimension limits, byte limits, many-image thresholds, and
placeholder fallbacks.
