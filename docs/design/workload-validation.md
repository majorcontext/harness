# Phase-2.5 workload validation: proving real fleet workloads on the boxes stack

## Motivation

The boxes stack (a GKE/gVisor fleet control plane) is the planned
replacement for Modal as harness's deployment target. Before that
replacement decision is final, real fleet workloads must run on the new
stack and be measured against the Modal baseline this repo already
documents (`docs/deploy-modal.md`). This is the write-up for the harness
that proves it: `e2e/workload` (Go, build tag `e2e`), one test per
acceptance row, each self-cleaning, each printing the numbers it measured,
each skipping loudly — never failing — when its own precondition is not
yet met.

It is a build spec in the same register as `docs/design/fleet-model.md`
and `docs/design/managed-processes.md`: an acceptance table, a pass bar
per row, and a stated boundary of what this repo's suite owns versus what
the infra/deploy side (`internal/`, `infra/`, `deploy/`, `.github/` —
explicitly out of scope for this change) must still wire up. Most of the
table is **not yet runnable**: the boxes service has no deployed instance
this suite can reach, and the harness image itself is not yet published
to GAR (see row 1's finding below). The suite is built and ready now so
that the moment each precondition lands, the matching row runs with no
further code change.

## The acceptance table

| Row | What it proves | Status |
|---|---|---|
| 1 | The real, digest-pinned harness image (GAR `us-central1-docker.pkg.dev/neptune-agent-sbx/workspace-images/web`) spawns and `harness serve` answers on 4096. | BLOCKED-ON-image-publish (see finding below); the non-root-USER precondition sub-check is RUNNABLE-NOW. |
| 2 | Real dev-workload timing under gVisor vs Modal: `pnpm install` (warm), a build, a test run — timed. | BLOCKED-ON-boxes-service, BLOCKED-ON-baked-repo |
| 3 | A real harness agent session + goal loop doing file edits/tool calls/git on the baked repo; session log fsyncs in DEFAULT mode (not the Modal volume workaround); no wedge. | BLOCKED-ON-boxes-service |
| 4 | Rotation (compressed max-age) mid-session; disk carries the session across via ADOPT. | ADOPT half: BLOCKED-ON-boxes-service only. Rotation half: BLOCKED-ON-log-rotation-mechanism (no such mechanism exists anywhere in this repo or the named boxes-service shapes yet). |
| 5 | Egress: box→bifrost, box→GitHub, box→npm through Cloud NAT static IPs; confirm NAT IPs are the observed source. | Reachability half: BLOCKED-ON-boxes-service. NAT-IP-match half: additionally BLOCKED-ON-NAT-IP-list (must come from infra config). |
| 6 | Kill-resume on a REAL spawn (SIGKILL the lifecycle worker mid-real-spawn), not the fake-backed demo. | BLOCKED-ON-infra-chaos-hook (needs cluster-level access this suite deliberately does not carry). |

"RUNNABLE-NOW" means: reachable with today's merged pieces, no other
team's work required. Every other row is scaffolded — client, test body,
cleanup, skip messages — and starts running the moment its precondition
clears, with no code change beyond setting the named env var(s).

## Row 1 finding: the harness image is not yet published

Run today, against real Google Artifact Registry credentials
(`gcloud auth print-access-token`), project `neptune-agent-sbx`:

```
$ gcloud artifacts repositories list --project=neptune-agent-sbx
REPOSITORY  FORMAT  LOCATION     ...
boxes       DOCKER  us-central1  ...   (created 2026-08-08, 0 images)
```

The `workspace-images` repository the acceptance table names does not
exist in this project. Only `boxes` exists, and it is empty. Running
`TestPrecondition_HarnessImageNonRoot` against the default image
reference confirms this over the real Docker Registry HTTP API v2 (not
just `gcloud`'s own listing):

```
$ go test -tags=e2e ./e2e/workload/... -run TestPrecondition_HarnessImageNonRoot -v
--- SKIP: TestPrecondition_HarnessImageNonRoot (0.82s)
    precondition not met: harness image not yet published to GAR
    (registry=us-central1-docker.pkg.dev repository=neptune-agent-sbx/workspace-images/web
    reference=latest: registry API status 404:
    {"errors":[{"code":"NAME_UNKNOWN","message":"Repository \"workspace-images\" not found"}]})
```

This is a SKIP, not a FAIL: an absent image is a publish-pipeline gap, not
the security defect this precondition exists to catch. Once
`workspace-images/web` (or whatever repository the devcontainer build
publishes to) has an image, re-running the same command performs the real
check this row exists for: pull the manifest and image config over the
GAR REST API, and fail loudly if `.config.User` is empty — the sandbox
review's own finding is that a `runAsNonRoot`-enforcing scheduler (gVisor
on GKE) refuses to start a container image with no declared non-root
user, and that refusal must be caught here, at row 1, not read off a
mysterious spawn timeout three rows later.

## Pass bar per row

- **Row 1**: `GET /health` returns 200 with a non-empty `version` within
  `spawnPollDeadline` (5 minutes) of the spawn request. The non-root-USER
  precondition sub-check: `.config.User` non-empty, checked independent of
  a live spawn.
- **Row 2**: each phase (install/build/test) completes with `turn.end
  outcome=completed`; the recorded duration is **within roughly 1.5x** of
  the equivalent phase's duration on Modal (`docs/deploy-modal.md`'s
  deployment shape) — a workload three times slower on the replacement
  stack would sink the migration case even if every other row passes.
  Both runs' JSON artifacts (`testdata/timing-*.json`, or
  `TIMING_ARTIFACT_PATH`) are the diff input; this suite writes one side
  of that diff, not the comparison itself.
- **Row 3**: the goal reaches `goal.achieved` (not `parked`, not
  `max_turns_exceeded`) within the test's own deadline, the box's `/health`
  reports `session_sync: "fsync"` (never `"volume"` — that mode exists
  specifically to route around a Modal Volume v2 fsync deadlock the boxes
  stack's own storage must not reproduce), and the session's final `state`
  is not `busy` (no wedge).
- **Row 4**: ADOPT half — message count and `seq` after a kill+respawn
  under the same box name are `>=` their pre-kill values (never less; see
  `docs/design/fleet-model.md` §4). Rotation half — no pass bar exists yet
  because no rotation mechanism exists yet to point a pass bar at.
- **Row 5**: bifrost, GitHub, and npm each answer a real HTTPS request
  (non-empty, non-`000` status code) from inside the box. When
  `NAT_STATIC_IPS` is configured, the observed source IP from the IP-echo
  check must be a member of that set — the whole point of the row.
- **Row 6**: a spawn request issued around a lifecycle-worker kill either
  succeeds on its own or fails in a way a same-name retry immediately
  recovers from; the resulting box reaches `running` and answers
  `/health`. No half-spawned, no-path-forward limbo state.

## How to run it

```bash
# Everything reachable right now (no BOXES_URL needed for the GAR check):
go test -tags=e2e ./e2e/workload/...

# Just the row-1 image precondition:
go test -tags=e2e ./e2e/workload/... -run TestPrecondition_HarnessImageNonRoot -v

# Against a live boxes service, once deployed:
export BOXES_URL=https://boxes.example.internal
export BOXES_TOKEN=...
export HARNESS_IMAGE_REF=us-central1-docker.pkg.dev/neptune-agent-sbx/workspace-images/web@sha256:...
go test -tags=e2e ./e2e/workload/... -v
```

`go test ./...` and `go build ./...` (no `-tags=e2e`) never build this
package — `go list ./e2e/...` without the tag does not even enumerate
`e2e/workload`, since every file in it carries `//go:build e2e` and Go
treats a directory with zero buildable files under the active constraints
as no package at all. CI stays exactly as fast as before this suite
existed.

### Env vars

| Var | Used by | Purpose |
|---|---|---|
| `BOXES_URL`, `BOXES_TOKEN` | rows 1-6 | The deployed fleet control plane's base URL and bearer token. |
| `HARNESS_IMAGE_REF` | precondition, rows 1-3, 5, 6 | Overrides the default GAR image reference. |
| `GAR_ACCESS_TOKEN` | precondition | Overrides `gcloud auth print-access-token` (for CI using a pre-minted token). |
| `TIMING_ARTIFACT_PATH` | row 2 | Overrides the default `testdata/timing-<ts>.json` output path. |
| `ROTATE_LOGS_CMD` | row 4 | Infra-owned shell hook to trigger log rotation mid-test. |
| `EGRESS_IP_ECHO_URL`, `BIFROST_URL` | row 5 | Override the IP-echo and bifrost endpoints egress is checked against. |
| `NAT_STATIC_IPS` | row 5 | Comma-separated Cloud NAT static IPs; enables the source-IP-match assertion. |
| `BOXES_CHAOS_KILL_CMD` | row 6 | Infra-owned shell hook that kills the boxes-service lifecycle worker. |

## What each row proves about the Modal-replacement decision

- **Row 1** proves the image itself is schedulable on a
  `runAsNonRoot`-enforcing platform at all — the hard gate every other row
  sits behind, which is why it is the one precondition wired to run
  without any other piece of the stack deployed.
- **Row 2** proves the replacement is not a regression on the workload
  that actually matters (an agent doing real dependency installs, builds,
  and test runs) — the single number most likely to sink or save the
  migration case on its own.
- **Row 3** proves the harness engine's own durability contract (fsync,
  goal loop, tool calls) holds on the new storage backend WITHOUT needing
  the Modal-specific `session_sync: "volume"` escape hatch
  (`docs/deploy-modal.md`) — i.e., the new stack's storage does not
  reproduce the FUSE-fsync deadlock that escape hatch was built for.
- **Row 4** proves a box's compute is genuinely disposable (cattle) while
  its session history is genuinely durable (pets) — the cattle/pets split
  `docs/design/fleet-model.md` is built around — under BOTH ordinary
  restart (ADOPT) and log-rotation churn, not just one.
- **Row 5** proves the network posture (default-deny egress through a
  known, allow-listable IP range) that lets bifrost's credential-injecting
  proxy model work at all actually holds on the new platform, not just on
  paper.
- **Row 6** proves the control plane itself — not just the harness
  process — survives a mid-operation crash without leaving a box stuck
  forever; the infra-side sibling to the in-repo `e2e/` package's own
  SIGKILL-durability tests, which only ever exercise the harness process
  against a fake provider.

## Design notes for maintainers

- `e2e/workload/client_boxes.go` (the fleet control plane's own
  `POST /v1/boxes`/spawn/GET/events/hibernate surface) is **provisional**:
  no OpenAPI spec for that service exists in this repo. It encodes the
  shapes this table names, isolated to one file specifically so the
  adjustment, once the real spec lands, stays contained.
- `e2e/workload/client_harness.go` (the `harness serve` API a box exposes
  on 4096) mirrors the ALREADY-SPECIFIED `server/openapi.yaml` and should
  not need to change independent of that spec.
- Every row drives its box through a REAL harness agent turn (a directive
  plus the bash tool) rather than a lower-level exec call, because neither
  API surface this suite has a spec for exposes one. This is a deliberate
  choice, not a limitation worked around: it measures what a fleet
  workload actually pays (agent dispatch overhead included), not a
  synthetic lower bound.
- `spawnPollInterval`/`spawnPollDeadline` (`e2e/workload/row1_spawn_test.go`)
  are this suite's one deadline-bounded poll loop pattern, used for every
  "wait for the box to reach a state" wait — the documented, allowed
  exception under AGENTS.md's Testing section for cross-process e2e, since
  no in-process channel can cross the box's own process boundary.
