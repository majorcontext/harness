# Image clamp

Read the root AGENTS.md. Read `provider/AGENTS.md` before changing adapter limits.

- `Clamp` repairs a throwaway request. It never mutates canonical history.
- Return the original slice without allocation when no image changes.
- Keep output deterministic so prompt caches stay stable.
- Enforce both dimension and encoded-byte limits.
- Reject absurd dimensions or pixel counts before a full decode.
- Use the text placeholder when decode or downscale is impossible.
- Keep the decode-memory guards. Never rewrite the durable source blob.
- The caller decides whether to recurse into tool results.
- Test with small generated fixtures: copy-on-write, determinism, limits, placeholders.
