# GET /git/changes

`GET /git/changes` answers a box's git diff for the boxes web console's
Changes panel, mirroring `GET /process`'s role for box-local process state.
See `server/git_changes.go` and `server/openapi.yaml` for the wire contract.

## Constant subprocess cost

A large untracked directory (an unignored `node_modules/`) or a large
refactor must not make one request's cost grow with the number of changed
files: that would exhaust the request's own deadline, and each console
poll would start proportionally many git processes. Untracked files are
folded into the SAME subprocess pass that computes tracked changes, so
total subprocess count stays constant:

1. Copy the real index (`git rev-parse --git-path index`, resolved so
   this works inside a git worktree, not just a plain clone) to a private
   temporary file. A missing real index (an unborn repository) leaves the
   copy unwritten; git treats a `GIT_INDEX_FILE` path that doesn't exist
   as a fresh empty index.
2. List untracked paths with `git ls-files --others --exclude-standard
   --directory`: a wholly untracked directory is one entry there, not one
   per file, which keeps git's own pathspec matching (quadratic in
   pathspec count) off a large untracked tree. Each entry is Lstat-checked
   for a nested git repository somewhere inside it; a contaminated
   directory is expanded to its own files instead, skipping that
   repository's subtree.
3. Stage that list as intent-to-add in the private index copy only, via
   `git add -N --pathspec-from-file=- --pathspec-file-nul -c
   core.splitIndex=false` (the config keeps `add -N` from writing a
   shared-index file into the real repository). A path that vanishes
   between steps 2 and 3 fails that whole batch, so this retries once
   against a fresh listing.
4. Run `--numstat`, `--name-status`, and the patch diff against that same
   copy. Each now reports tracked AND untracked files together — an
   intent-to-add entry has no blob content, so diffing it against the
   base tree shows every line as added, exactly like an "added" status.

The real `.git/index` is only ever read (a plain file copy), never
written by any git command — see "Never writing the index" below.

## Bounded memory: reading the patch

The patch diff's stdout can be arbitrarily large. Reading it into an
unbounded buffer before checking the cap means one request's memory use
scales with the size of the underlying diff, not with the response it
actually returns.

`runPatchCapped` reads at most `patchCap+len(diffGitMarker)` bytes via
`io.LimitReader` — the small overread lets a file whose content ends
exactly at the cap still be recognized as a whole-file boundary, since
confirming one needs the following file's marker fully in view. Reading
that many bytes means more output remains; the function then kills the
subprocess rather than draining it, and cuts the captured prefix back to
the last whole-file boundary within `patchCap` bytes — never mid-hunk.
`files` comes from the separate, already-bounded `--numstat`/
`--name-status` calls, so it stays complete regardless of patch
truncation.

The cap is ~1 MiB (`gitChangesPatchCap = 1<<20`): large enough for a
typical PR-sized diff, small enough to keep a single response bounded.

## Never writing the index

Porcelain `git diff` refreshes the index (`refresh_index_quietly`) when a
file's stat info changed but its content did not — rewriting
`.git/index` through `index.lock` even with `GIT_OPTIONAL_LOCKS=0` set.
An agent running `git add` or `git commit` at the same moment would then
fail with `Unable to create '.git/index.lock': File exists`.

Running every diff against the private index copy (above) means this
endpoint's own git commands never write the real index at all (they read
it once, as a plain file copy), so this race cannot happen structurally —
not merely suppressed with `diff.autoRefreshIndex=false` (which is also
set, defensively, on both `add -N` and the diffs).

## Command execution surfaces neutralized

A git repository can configure several commands that run automatically
while diffing a working-tree file. Every one is disabled on every
invocation, not just the patch diff:

- `diff.external` / `*.textconv`: `--no-ext-diff --no-textconv`.
- A clean/process content filter driver (e.g. git-lfs's own
  `filter.lfs.clean`/`filter.lfs.process`, at any config scope):
  discovered once per request via `git config --get-regexp
  '^filter\..*\.(clean|process)$'` and neutralized per driver with `-c
  filter.<d>.clean= -c filter.<d>.process= -c filter.<d>.required=false`.
  A driver name may itself contain a dot (`filter.a.b.clean`); the split
  takes everything before the LAST `.`, not the first.
- `core.fsmonitor`: `-c core.fsmonitor=false`.
- An ambiguous bare-repository layout: `-c safe.bareRepository=explicit`.
  No command run here uses hooks, so `core.hooksPath` is not set.
- A dirty submodule (which can be arbitrarily large and is not the
  superproject's own uncommitted content): `--submodule=short
  --ignore-submodules=dirty`.
- A partial clone's lazy object fetch: `GIT_NO_LAZY_FETCH=1` (git 2.44+;
  harmless on older git, which may still lazy-fetch on a partial clone —
  the fleet's own clones are `--depth 1`, never partial, so this is
  belt-and-suspenders here).

`GIT_LITERAL_PATHSPECS=1` additionally keeps a pathspec built from a real
file's name (e.g. `b*.txt`) from being reinterpreted as a glob.

## Repository-root resolution

`dir` resolves like `POST /session`'s `workdir` (`resolveWorkDir`), then
is independently re-validated (`verifyDirWithinRoots`): stated to exist
and be a directory (else 400, rather than a git subprocess failing to
chdir), and symlink-resolved and re-checked against every workspace root
(a symlink planted under an allowed root must not point outside all of
them). Every git command then runs at that work tree's root
(`git rev-parse --show-toplevel`, bounded by `GIT_CEILING_DIRECTORIES` at
the matched root's parent), not at `dir` itself: `dir` may name a
subdirectory, and running commands there directly instead would produce
mixed-relative paths (untracked files outside the subdirectory would be
invisible; a per-file pathspec would not match anything there). The
response's own `dir` field still echoes the originally resolved `dir`.

## Base resolution edge cases

- Unborn HEAD (no commit yet): `scope=uncommitted` diffs against
  `git hash-object -t tree /dev/null` (the repository's own empty tree,
  correct for either the SHA-1 or SHA-256 object format) and reports
  `head: ""`. `scope=branch` has no HEAD to take a merge-base from, so it
  is `409 no_base`.
- A stale `origin/HEAD` (left pointing at a branch a `fetch --prune`
  already removed): `defaultBranchRef` verifies each candidate's target
  actually resolves before accepting it, so this falls through to
  `origin/main`/`origin/master` instead of 500ing.
- No common ancestor (an orphan branch, or a shallow clone too shallow to
  reach it): `git merge-base` exits 1 with no output; mapped to `409
  no_base` rather than surfaced as a raw subprocess failure.

## Accepted limitations

- The patch's JSON encoding disables HTML escaping (`writeJSONNoEscapeHTML`,
  a local counterpart to `writeJSON`) so a diff of HTML/JSX doesn't
  balloon past its documented byte cap; this endpoint's `patch` field is
  the only reason a server response needs that.
- A file name that isn't valid UTF-8 is not byte-accurate in the JSON
  response: `encoding/json` substitutes the Unicode replacement
  character. The contract's response shape is JSON strings for paths;
  round-tripping arbitrary bytes would need a different (e.g.
  base64-encoded) shape, which is out of scope here.
- `--numstat`, `--name-status`, and the patch diff are three separate git
  processes reading the same working tree in quick succession, not one
  atomic snapshot. Folding untracked files into the same private-index
  pass (above) narrows the window considerably, but does not eliminate
  it — full atomicity would need a lock on the repository, which this
  endpoint must never take.
