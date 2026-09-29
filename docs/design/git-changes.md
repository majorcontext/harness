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
   as a fresh empty index. A committed repository whose index is missing
   instead gets its private index seeded from `HEAD` (`git read-tree HEAD`),
   the state `git reset --mixed` would produce: an empty index there would
   report every unchanged tracked file as deleted or modified. After copying
   or seeding the private index, refresh it with `git -c
   core.splitIndex=false update-index -q --unmerged --refresh` and filter
   drivers neutralized. This refresh tolerates unresolved merge entries.
   `ls-files --others` reads the same private index, so it agrees with the
   diffs on what is tracked.
2. List untracked paths with `git ls-files --others --exclude-standard`
   (no `--directory`): an entry ending in `/` is exactly a nested git
   repository, which git itself refuses to descend into. `untrackedEntries`
   partitions this list into an `add -N` exclude pathspec per nested
   repo, plus one per untracked file over the 2 MiB large-file cutoff
   (below) — everything else is left for `add -N` to add.
3. Stage the private index copy with ONE `git add -N` (`-c
   core.splitIndex=false`, so it never writes a shared-index file into
   the real repository; a copied index that is already split still
   resolves its `link` extension, because git looks for
   `sharedindex.<hash>` in `$GIT_DIR`, not next to `GIT_INDEX_FILE`), fed pathspec `.` plus an exclude per nested repo
   or large file over `--pathspec-from-file`'s stdin, not argv — tens of
   thousands of excludes would otherwise risk the OS argument-size limit.
   Passing pathspec `.` plus an exclude per nested repo or large file —
   rather than one include pathspec per ordinary file — is what keeps
   this a single subprocess regardless of file count: git's own pathspec
   matching is quadratic in pathspec count, so one include pathspec per
   file made this step alone take 24 s at 50,000 untracked files. Git
   itself then decides .gitignore and repository boundaries for the
   files `.` does cover, rather than this package re-deriving them file
   by file. A path that no longer exists by the time `add -N` runs is
   simply outside what `.` matches; an exclude naming a path that no
   longer exists is a no-op — neither is an error, so this needs no
   retry.
4. Run `--numstat`, `--name-status`, and the patch diff against that same
   copy. Each now reports tracked AND untracked files together — an
   intent-to-add entry has no blob content, so diffing it against the
   base tree shows every line as added, exactly like an "added" status.

The real `.git/index` is only ever read (a plain file copy), never
written by any git command — see "Never writing the index or objects"
below.

## Large untracked files

An untracked file over 2 MiB (`untrackedLargeCutoff`, matching opencode's
own snapshot cutoff) is excluded from `add -N` the same way a nested repo
is, and given its own `gitChangeFile` directly: status `"added"`,
`additions`/`deletions` 0, `large: true`, and no content in `patch` — the
same shape `binary` already uses for a file whose content isn't
diffable, here applied to a plain-text file whose size alone makes
diffing it not worth the request's own cost.

This large-file rule only applies to a path with no match in
`baseTreeish`. One that already exists there under the same path (most
commonly a large tracked file `git rm --cached`'d) is excluded from
`add -N` exactly the same, but reported here NOT AT ALL: leaving it out
of the temp index is exactly its real state, so the normal numstat/
name-status diff already reports its true status (typically "deleted")
on its own. Reporting it here too would duplicate that entry.
`gitChangeSet` decides this after running the normal diff, not before: a
candidate large path is looked up against the diff's own already-parsed
file paths, a plain in-memory set membership check with no separate git
call and no line-oriented protocol to get a path's own newline wrong.
Rename detection (`-M`) can pair a large old path with a small new one
(`git rm --cached`'d `p.txt`, its own content similar enough to a new,
under-cutoff `q.txt`, reports `R p.txt q.txt`); the membership check marks
both the rename's new and old path as seen, so `p.txt` isn't ALSO reported
as a synthetic `large:true` "added" entry alongside the rename.
A large candidate that no longer exists when the diff finishes (removed
after `ls-files` ran) is dropped, the same as an ordinary untracked file
that `add -N` no longer finds.

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

`ls-files`, `--numstat`, and `--name-status` can't be truncated the same
way — `files` must stay complete — so `gitOutCapped` bounds each of their
own stdout to `gitChangesMetadataCap` (32 MiB) instead: exceeding it kills
the subprocess and answers `409 too_many_changes`, the same scale-ceiling
response a deadline produces, rather than letting an unusually large
change set's metadata alone grow a response's memory without bound.

Filter-driver discovery (`git config --get-regexp`) has its own, much
smaller cap, `gitFilterDiscoveryCap` (64 KiB). Each discovered driver
becomes three `GIT_CONFIG_KEY_i`/`GIT_CONFIG_VALUE_i` override pairs in
every later diff's environment, and the environment shares `ARG_MAX` (2 MiB
on Linux) with argv, so a config that fits the 32 MiB metadata cap could
still make the diff's exec fail with `E2BIG`. At 64 KiB of discovery output
the overrides stay under about 450 KiB; exceeding it answers `409
too_many_changes`.

## Never writing the index or objects

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

Staging a new file's intent-to-add entry also makes git write the empty
blob's loose object (`e69de29…`), which would otherwise land in the real
`.git/objects`. `GIT_OBJECT_DIRECTORY` points every object write at a
private, request-scoped directory instead;
`GIT_ALTERNATE_OBJECT_DIRECTORIES` points reads at the real objects
directory (resolved via `git rev-parse --git-path objects`, the same
worktree-aware pattern as the index), so a diff against real history
still resolves every blob it needs. Like `GIT_CEILING_DIRECTORIES`, this
is itself a colon-separated git path list, but unlike it, git's alternates
parsing DOES honor a double-quoted, backslash-escaped entry (the same
syntax `objects/info/alternates` accepts) — `gitQuotePathListEntry` applies
that quoting, so a repository whose own path contains a colon still
resolves every object instead of failing with "bad object".

## Command execution surfaces neutralized

A git repository can configure several commands that run automatically
while diffing a working-tree file. Every one is disabled on every
invocation, not just the patch diff:

- `core.hooksPath`: `-c core.hooksPath=/dev/null`. `add -N` (like `add` and
  `commit`) runs the repository's own `post-index-change` hook after
  writing the index, so a repo-controlled hooksPath would otherwise run
  arbitrary code as this process on every request.
- `diff.external` / `*.textconv`: `--no-ext-diff --no-textconv`.
- A clean/process content filter driver (e.g. git-lfs's own
  `filter.lfs.clean`/`filter.lfs.process`, at any config scope):
  discovered once per request via `git config --get-regexp
  '^filter\..*\.(clean|process)$'` and neutralized per driver by setting
  `filter.<d>.clean` and `filter.<d>.process` to empty and
  `filter.<d>.required` to `false`. A driver name may itself contain a dot
  (`filter.a.b.clean`); the split takes everything before the LAST `.`,
  not the first. The overrides go in `GIT_CONFIG_COUNT` plus
  `GIT_CONFIG_KEY_i`/`GIT_CONFIG_VALUE_i` pairs, set after the inherited
  environment is stripped, not as `-c filter.<d>.clean=`: git splits `-c
  k=v` at the first `=`, so a driver named `x=y` would never be overridden
  and would run.
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

Every subprocess starts from harness's own environment minus git's
repository-local variables (`git rev-parse --local-env-vars`: `GIT_DIR`,
`GIT_INDEX_FILE`, `GIT_OBJECT_DIRECTORY`, `GIT_CONFIG_PARAMETERS`, and the
rest). A git hook exports `GIT_DIR` and `GIT_INDEX_FILE`; inherited, they
override the command's working directory, so the endpoint would read
another repository's index while `--show-toplevel` still named the
requested one. `gitBaseEnv` removes them, and each command adds back only
the values this endpoint sets itself. It also removes the pathspec-mode
variables (`GIT_LITERAL_PATHSPECS`, `GIT_GLOB_PATHSPECS`,
`GIT_NOGLOB_PATHSPECS`, `GIT_ICASE_PATHSPECS`): an inherited
`GIT_LITERAL_PATHSPECS=1` turns `add -N`'s `:(exclude,literal)` magic
into a literal path that matches nothing, and the request fails.

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

An omitted `dir` is the process's own working directory, as for `POST
/session`'s `workdir`, and like it is not checked against the workspace
roots: the operator chose it. It gets no root-derived
`GIT_CEILING_DIRECTORIES` and no `repoRoot` re-check. Only an explicit `dir`
is confined to the roots.

`GIT_CEILING_DIRECTORIES` is itself colon-separated with no escape for a
colon in a path, so a workspace root under a path component containing
one defeats it silently: git ignores the malformed ceiling and keeps
walking up past the intended boundary into an enclosing repository. The
resolved `repoRoot` is re-checked with the same `verifyDirWithinRoots`
used on `dir`, so that escape is caught regardless of what confused the
ceiling.

`--show-toplevel`'s own output is trimmed with `strings.TrimSuffix(out,
"\n")`, not `TrimSpace`: a repository whose real path ends in a space is a
legitimate, if unusual, directory name, and `TrimSpace` would silently
drop it, making the endpoint report `not_a_git_repo` for a real repository.

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
- A deadline mid-call during `HEAD` or default-branch resolution: both
  `rev-parse HEAD` and `defaultBranchRef` check `ctx.Err()` on failure and
  route a killed subprocess through `writeGitErr` (`409 too_many_changes`),
  rather than reading the failure as an unborn `HEAD` or a missing default
  branch (`409 no_base`). `scope=branch` with an unborn `HEAD` returns
  before ever resolving the empty tree, so that resolution now runs only
  for `scope=uncommitted`'s own unborn-`HEAD` case — no longer a wasted
  subprocess call on every branch-scope request against a commit-less repo.

## A change set too large for the request's own deadline

An unusually large change set (order 100,000+ untracked files) can still
exhaust the request's own bounded, linear-time deadline: the cost per
file stays constant, but the total is not unbounded. `writeGitErr` checks
`ctx.Err()` on any git-call failure and answers `409 too_many_changes`
instead of `500` when the deadline itself is what killed the subprocess —
a scale ceiling, not a server fault. `gitChangesTimeout` is a package var,
not a const, so a test can shrink it and deterministically reach this
path without needing hundreds of thousands of real files.

The git work itself is bounded to `gitChangesTimeout -
gitChangesResponseMargin` (28s of the nominal 30s), not the full budget:
marshaling and writing the response still needs a couple of seconds after
the deadline fires, and boxes' own proxy timeout for this route is close
to `gitChangesTimeout` too. Spending the whole budget on git work would
risk the client giving up before this endpoint's own 409 reaches it.

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
