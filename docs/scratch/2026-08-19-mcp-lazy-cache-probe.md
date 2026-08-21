# MCP lazy tool exposure — measured go/no-go probe

Date: 2026-08-19
Branch: `fx/mcp-lazy-probe`
Status: **COMPLETE — recommendation: GO WITH CONDITIONS**

Measurement only. Zero production code changed; every file added by this
probe lives under `docs/scratch/`. Credentials are referred to by env var
NAME only and never printed.

## Question

Harness advertises EVERY tool of every connected MCP server in full (name +
description + full JSON schema) on EVERY request. Lazy loading (a
search/select pair, schemas loaded one at a time) saves prefix tokens but
VARIES the tool prefix between requests, which may forfeit provider prompt
caching. A stable fat prefix that caches could beat a thin prefix that
misses. This probe measures both before anything is implemented.

## Headline result

| | fat (today) | thin catalog |
|---|---|---|
| tools-prefix tokens billed per turn | 87,959 | 8,400 |
| turn-2 billing rate | cache read | cache read |
| turn-2 TTFT | 1.114–1.557s | 0.781–0.838s |

Varying the tool array between turns costs a **total** cache miss (0 read,
86,184 re-created), not a partial one. But an **append-only** thin catalog
keeps a full cache read on the stable prefix while billing only the newly
loaded schema as fresh input. That combination is what makes this a GO.

---

## 1. Static: what the fat prefix actually costs

Four real MCP servers from this box's `HARNESS_MCP_SERVERS` (gateway URLs +
`Authorization` headers; values never printed) were connected with the
repo's own MCP client (`mcp.NewClient` / `HTTPTransport` /
`ListAllTools`) via the throwaway program
`docs/scratch/probe/toolsize/main.go`.

Sizes are measured on the exact wire shape the anthropic adapter emits per
tool (`provider/anthropic/transcode.go`'s `apiToolDef`: `name`,
`description`, `input_schema`), with harness's real
`mcp__<server>__<tool>` name namespacing applied.

Command:

```
$ go run ./docs/scratch/probe/toolsize -dump /tmp/mcpdump
```

Output:

```
CONNECT braintrust url=https://boxes.meetneptune.dev/v1/mcp/gateway/braintrust headers=[Authorization] (values redacted)
CONNECT linear url=https://boxes.meetneptune.dev/v1/mcp/gateway/linear headers=[Authorization] (values redacted)
CONNECT neon url=https://boxes.meetneptune.dev/v1/mcp/gateway/neon headers=[Authorization] (values redacted)
CONNECT boxes-orchestration url=https://boxes.meetneptune.dev/v1/mcp/orchestration headers=[Authorization] (values redacted)

=== FAT PREFIX (what harness ships today: name+description+full input schema) ===
server                  tools      nameB      descB      schemaB        wireB    ~tokens
braintrust                 39       1399      36146       113212       152952      38238
linear                     63       1774      10480        62171        77376      19344
neon                       35        989      51998        22260        80112      20028
boxes-orchestration        10        407       1880         2798         5538       1384
TOTAL                     147       4569     100504       200441       315978      78994

combined tools array marshalled: 315975 bytes (~78993 tokens at 4 B/token)

=== THIN CATALOG ALTERNATIVE (name + one-line description, no schemas) ===
thin catalog entries:            147
thin one-line description bytes: 9263
thin catalog marshalled:         18196 bytes (~4549 tokens)
search/select tool pair:         908 bytes (~227 tokens)
thin TOTAL (catalog + pair):     19104 bytes (~4776 tokens)

DELTA fat->thin: 296871 bytes saved (~74217 tokens), 94.0% reduction
```

Per-server tool counts confirmed independently from the dumped JSON:

```
$ for f in /tmp/mcpdump/*.tools.json; do echo "$(basename $f): $(python3 -c "import json,sys; print(len(json.load(open('$f'))))") tools"; done
boxes-orchestration.tools.json: 10 tools
braintrust.tools.json: 39 tools
linear.tools.json: 63 tools
neon.tools.json: 35 tools
```

### Reading of the static numbers

- **Input schemas dominate**: 200,441 B of 315,978 B (63%) is schema, versus
  4,569 B (1.4%) of names. Descriptions are 100,504 B (32%).
- The `bytes/4` estimate (78,994 tokens) is close to what Anthropic actually
  billed for the same array (87,959 tokens, §2) — the estimate runs ~10%
  low, so treat `bytes/4` as a floor, not a ceiling.
- Two servers carry pathological payloads for opposite reasons: braintrust
  is schema-heavy (113,212 B of schema across 39 tools), neon is
  description-heavy (51,998 B across 35 tools).

---

## 2. Live: what varying the prefix costs in cache terms

Live calls **succeeded** from this box. The anthropic provider is reachable
through the box's configured Bifrost base URL, credentialed from env var
`BIFROST_API_KEY` (value never printed, never committed).

Reachability check:

```
$ curl -sS -w '\nHTTP %{http_code} ttfb=%{time_starttransfer}s\n' \
    -X POST https://bifrost.meetneptune.dev/anthropic/v1/messages \
    -H "x-api-key: $BIFROST_API_KEY" \
    -H "anthropic-version: 2023-06-01" \
    -H "content-type: application/json" \
    -d '{"model":"anthropic/claude-haiku-4-5-20251001","max_tokens":16,"messages":[{"role":"user","content":"say ok"}]}'
{"id":"msg_011CeEXjbuammAnMD6x4mmfG","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-haiku-4-5-20251001","stop_reason":"end_turn","usage":{"input_tokens":9,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"output_tokens":4,"service_tier":"standard"}}
HTTP 200 ttfb=0.088838s
```

### Method

`docs/scratch/probe/cacheprobe/main.go` sends two turns per scenario with
identical message history, 30s apart, streaming, and reports
`cache_creation_input_tokens` / `cache_read_input_tokens` / TTFT per turn.

Two methodology points matter, both learned the hard way:

1. **Cold-cache nonce.** A first (discarded) run showed `cache_read` on
   turn 1 of every scenario — an earlier timed-out invocation had already
   warmed those exact prefixes, contaminating the measurement. The probe
   now injects a per-run/per-scenario nonce into the FIRST tool's
   description (`coldify`), which invalidates the whole tools prefix by
   construction, so every scenario provably starts cold. Every number
   below comes from a run whose turn 1 reads `cache_read=0`.
2. **Two independent cold runs** (`-nonce cold-run-1`, `-nonce cold-run-2`)
   were performed; they agree to the token on every scenario.

### Run 1

```
$ go run ./docs/scratch/probe/cacheprobe -tools /tmp/mcpdump -gap 30s -nonce cold-run-1
endpoint: https://bifrost.meetneptune.dev/anthropic/v1/messages
credential: from env BIFROST_API_KEY (value never printed)
model: anthropic/claude-haiku-4-5-20251001
turn gap: 30s

fat tool set:  147 tools
thin tool set: 149 tools (catalog entries + search/select pair)

==================== SCENARIO A stable-fat ====================
turn 1 tools: 147 (breakpoint idx 146) | turn 2 tools: 147 (breakpoint idx 146)
turn 1: input=349 cache_creation=87959 cache_read=0 output=4 ttft=2.357s total=2.401s
sleeping 30s...
turn 2: input=349 cache_creation=0 cache_read=87959 output=4 ttft=1.557s total=1.711s

==================== SCENARIO B varied (middle tool removed on turn 2) ====================
turn 1 tools: 147 (breakpoint idx 146) | turn 2 tools: 146 (breakpoint idx 145)
turn 1: input=349 cache_creation=87959 cache_read=0 output=4 ttft=1.784s total=1.799s
sleeping 30s...
turn 2: input=349 cache_creation=86184 cache_read=0 output=4 ttft=1.873s total=1.884s

==================== SCENARIO C stable-thin ====================
turn 1 tools: 149 (breakpoint idx 148) | turn 2 tools: 149 (breakpoint idx 148)
turn 1: input=349 cache_creation=8400 cache_read=0 output=4 ttft=1.298s total=1.328s
sleeping 30s...
turn 2: input=349 cache_creation=0 cache_read=8400 output=4 ttft=0.838s total=0.847s

==================== SCENARIO D append-only (breakpoint pinned to end of stable catalog) ====================
turn 1 tools: 149 (breakpoint idx 148) | turn 2 tools: 150 (breakpoint idx 148)
turn 1: input=349 cache_creation=8400 cache_read=0 output=4 ttft=0.758s total=0.772s
sleeping 30s...
turn 2: input=2126 cache_creation=0 cache_read=8400 output=4 ttft=0.922s total=0.930s
```

### Run 2 (independent cold repeat)

```
$ go run ./docs/scratch/probe/cacheprobe -tools /tmp/mcpdump -gap 30s -nonce cold-run-2
endpoint: https://bifrost.meetneptune.dev/anthropic/v1/messages
credential: from env BIFROST_API_KEY (value never printed)
model: anthropic/claude-haiku-4-5-20251001
turn gap: 30s

fat tool set:  147 tools
thin tool set: 149 tools (catalog entries + search/select pair)

==================== SCENARIO A stable-fat ====================
turn 1 tools: 147 (breakpoint idx 146) | turn 2 tools: 147 (breakpoint idx 146)
turn 1: input=349 cache_creation=87959 cache_read=0 output=4 ttft=2.348s total=2.356s
sleeping 30s...
turn 2: input=349 cache_creation=0 cache_read=87959 output=4 ttft=1.114s total=1.125s

==================== SCENARIO B varied (middle tool removed on turn 2) ====================
turn 1 tools: 147 (breakpoint idx 146) | turn 2 tools: 146 (breakpoint idx 145)
turn 1: input=349 cache_creation=87959 cache_read=0 output=4 ttft=2.296s total=2.341s
sleeping 30s...
turn 2: input=349 cache_creation=86184 cache_read=0 output=4 ttft=1.958s total=1.983s

==================== SCENARIO C stable-thin ====================
turn 1 tools: 149 (breakpoint idx 148) | turn 2 tools: 149 (breakpoint idx 148)
turn 1: input=349 cache_creation=8400 cache_read=0 output=4 ttft=0.915s total=0.941s
sleeping 30s...
turn 2: input=349 cache_creation=0 cache_read=8400 output=4 ttft=0.781s total=0.792s

==================== SCENARIO D append-only (breakpoint pinned to end of stable catalog) ====================
turn 1 tools: 149 (breakpoint idx 148) | turn 2 tools: 150 (breakpoint idx 148)
turn 1: input=349 cache_creation=8400 cache_read=0 output=4 ttft=0.749s total=0.814s
sleeping 30s...
turn 2: input=2126 cache_creation=0 cache_read=8400 output=4 ttft=0.764s total=0.794s
```

### Turn-2 summary (both runs identical on token counts)

| scenario | turn-2 cache_read | turn-2 cache_creation | turn-2 fresh input | turn-2 TTFT (run1 / run2) |
|---|---|---|---|---|
| A stable-fat | 87,959 | 0 | 349 | 1.557s / 1.114s |
| B varied | **0** | **86,184** | 349 | 1.873s / 1.958s |
| C stable-thin | 8,400 | 0 | 349 | 0.838s / 0.781s |
| D append-only | 8,400 | 0 | **2,126** | 0.922s / 0.764s |

### Findings

1. **A stable fat prefix does cache.** Turn 2 read all 87,959 tokens; zero
   re-creation. The brief's concern is real — this is a genuine benefit
   that a naive lazy loader would throw away.
2. **Varying the tool array is a TOTAL miss, not a partial one** (scenario
   B). Removing one tool from the middle re-created 86,184 tokens and read
   **zero**. Anthropic's cache is a prefix match, and tools serialize at
   the very front, so a single removed tool invalidates everything behind
   it. Note the miss is *worse than* a cold turn-1 in cost terms: you pay
   cache-write price on nearly the whole array again, every turn the set
   changes. A per-turn-varying tool set is the worst of both worlds.
3. **A stable thin catalog caches just as reliably, at 10.5x fewer tokens**
   (scenario C): 8,400 read versus 87,959, a 90.5% reduction in
   tools-prefix tokens billed per turn, with TTFT roughly halved (0.78–0.84s
   vs 1.11–1.56s).
4. **Append-only growth preserves the cache** (scenario D) — the decisive
   result. With the breakpoint pinned to the end of the stable catalog and
   the newly loaded schema appended AFTER it, turn 2 still read the full
   8,400-token catalog while billing the appended schema as 2,126 tokens of
   ordinary fresh input. This is the mechanism that makes lazy loading
   compatible with caching: **never mutate or reorder the catalog, only
   append behind the breakpoint.**

### Harness's real breakpoint placement was verified separately

Harness does not put `cache_control` on tools at all — it marks only the
last SYSTEM block (`provider/anthropic/transcode.go`: `out.System[n-1].CacheControl = ephemeral`).
Anthropic documents the cacheable prefix as ordered tools → system →
messages, so a system breakpoint should still cover the tools array ahead
of it. `docs/scratch/probe/sysbp/main.go` confirms that empirically using
harness's exact shape (breakpoint on system only, none on any tool):

```
$ go run ./docs/scratch/probe/sysbp -tools /tmp/mcpdump -gap 20s -nonce sysbp-1
endpoint: https://bifrost.meetneptune.dev/anthropic/v1/messages
credential: from env BIFROST_API_KEY (value never printed)
breakpoint placement: LAST SYSTEM BLOCK only (harness's real shape)

--- turn 1: fat tools, system breakpoint (cold) ---
input=330 cache_creation=87984 cache_read=0 ttft=2.261s

sleeping 20s...

--- turn 2: IDENTICAL tools + system (expect cache read covering the tools array) ---
input=330 cache_creation=0 cache_read=87984 ttft=1.746s

sleeping 20s...

--- turn 3: SAME system, ONE TOOL REMOVED (expect miss: tools precede the breakpoint) ---
input=330 cache_creation=86209 cache_read=0 ttft=2.239s
```

So both conclusions hold for real harness requests, not just for the
probe's tool-level-breakpoint arrangement: the tools array IS cached today
by the system breakpoint, and mutating it IS a full miss.

---

## 3. Recommendation: GO WITH CONDITIONS

**Go** — but only for an append-only, byte-stable-catalog design. A naive
lazy loader that swaps which schemas are resident per turn is a **no-go**:
scenario B measures it re-creating 86,184 tokens every turn it changes,
which is strictly worse than the fat prefix's 87,959-token cache READ,
because cache writes cost more than cache reads while reads cost a fraction
of base input.

### Quantified

Per-turn tools-prefix tokens, measured, for this box's 147-tool set:

| design | steady-state per-turn tools tokens | billing class |
|---|---|---|
| fat (today) | 87,959 | cache read |
| naive lazy (varies per turn) | 86,184 | cache **creation** — worst case |
| thin catalog, append-only | 8,400 + sum of loaded schemas | cache read + fresh input |

The thin catalog saves **79,559 tokens per turn of cache-read billing**
(90.5%) versus today. Each schema the model actually loads costs its own
size once as fresh input (2,126 tokens for the sample tool in scenario D)
and then rides the cache from the next turn onward if appended stably.

Break-even: the thin design loses only if a session loads more than roughly
`(87,959 - 8,400) / average_schema_size` tools. At the measured 2,126-token
average that is ~37 of 147 tools in a single session. Sessions that touch
more than a quarter of every connected server's tools are the exception,
not the rule — and the fat prefix's own 87,959 tokens are billed on every
turn regardless of whether any tool is used at all.

Secondary win: TTFT roughly halves (1.11–1.56s → 0.78–0.84s), measured on
a trivial 4-token completion where prefix handling dominates.

### Conditions (all three are load-bearing)

1. **The catalog must be byte-stable across a session.** Sort by
   `mcp__<server>__<tool>` and never reorder, never drop an entry when a
   schema loads, never rewrite a description. Scenario B is what a
   violation costs.
2. **Loaded schemas append AFTER the cache breakpoint**, never splice into
   the catalog. Scenario D is the measurement that this preserves the read.
3. **A server going away mid-session must not mutate the catalog.** Mark
   the entry unavailable at call time instead; removing it is scenario B.

### One-page design sketch

**Shape of the tools array on every request**

```
[ stable thin catalog: 147 entries, name + one-line description,
  minimal schema {"type":"object"}, sorted, byte-identical every turn ]
[ mcp_search_tools, mcp_load_tool ]        <- also stable
        ...............<= cache breakpoint lands at/behind here .......
[ loaded[0] full schema ]                  <- append-only, grows monotonically
[ loaded[1] full schema ]
```

**Flow.** The model sees every tool that exists (name + one-liner), so
discovery never regresses — this is a schema-loading optimization, not a
capability-hiding one. To call a tool whose schema is not resident it calls
`mcp_load_tool{names:[...]}`; the engine appends those schemas to the
resident set and the tool is callable from the next request onward.
`mcp_search_tools` exists for large catalogs where even one-liners are too
many to scan, and is optional for a 147-tool set.

**Interaction with the measured cache numbers.** Harness's existing
breakpoint is on the last system block, which sits AFTER tools in
Anthropic's prefix order — so with the catalog stable and loads appended,
the catalog would fall inside the cached prefix and the appended schemas
outside it only if the breakpoint moves. Given the §2 sysbp result (system
breakpoint caches the whole tools array), the simplest correct
implementation puts an explicit ephemeral breakpoint on the LAST CATALOG
ENTRY, exactly as scenario D did, and lets appended schemas bill as fresh
input until the next breakpoint refresh. That is the arrangement measured
at 8,400 read + 2,126 fresh.

**Degradation.** On a provider without prompt caching, the thin catalog is
a strict win (fewer tokens, no cache to lose). On the openai/openaicompat
routes it is also a strict win for the same reason; the `SessionKey`
affinity hint documented in AGENTS.md is orthogonal and unaffected.

**Proposed config keys** (naming only — deliberately NOT implemented here):

- `mcp_tool_exposure`: `"full"` (today's behaviour, the default until this
  is proven in a real session) | `"catalog"` (thin catalog + load tool).
- `mcp_catalog_description_chars`: int, cap for the one-line description
  (the probe used 160; 9,263 B of one-liners across 147 tools).
- `mcp_catalog_always_load`: `[]string` of tool-name globs whose full
  schemas are resident from turn 1, for tools a workflow always needs.
- `mcp_catalog_min_tools`: int threshold below which exposure stays full,
  so a 3-tool box does not pay for indirection.

### What this probe did NOT measure (honest gaps)

- **Model behaviour.** Every number here is billing and latency. Whether a
  model reliably calls `mcp_load_tool` instead of hallucinating arguments
  against a one-line description is a task-success question this probe does
  not answer, and it is the main risk to the "go".
- **One model only** (claude-haiku-4-5). Cache mechanics should be
  model-independent within Anthropic, but TTFT will differ.
- **5-minute TTL only.** The default ephemeral TTL; a 30s gap was used. Real
  sessions with longer think time may miss regardless of prefix stability.
- **Anthropic only.** The openai and openaicompat routes were not measured.

---

## 4. Zero production impact

### Diffs are confined to `docs/scratch/`

```
$ git diff --name-only main...HEAD
docs/scratch/2026-08-19-mcp-lazy-cache-probe.md
docs/scratch/probe/cacheprobe/main.go
docs/scratch/probe/sysbp/main.go
docs/scratch/probe/toolsize/main.go
```

No file outside `docs/scratch/` is added, modified, or deleted. The three
probe programs are `package main` commands with no importers; they read the
repo's `mcp` package but nothing in the repo reads them.

### Test suite

The full suite passes at this commit with zero FAIL lines, no test
excluded (final run at the end of this section). Getting there required
diagnosing two container facts rather than asserting them, both recorded
below because each one silently corrupts a naive `go test ./...` here.

**Go version.** The repo declares `go 1.25.5`; this container's default
toolchain is 1.26.5. On 1.26.5 a third test also fails
(`TestGarbageSessionIDsAreNotFound`, an empty path segment yielding 405
instead of 404 — a `net/http.ServeMux` behaviour change). Pinning the
declared toolchain fixes it:

```
$ GOTOOLCHAIN=go1.25.5 go test ./server/ -run TestGarbageSessionIDsAreNotFound -count=1
ok  	github.com/majorcontext/harness/server	0.017s
```

**The two bash tests are defeated by this box's PID 1, not by a broken
kill.** `TestBashTimeoutKillsWholeProcessGroup` and
`TestBashAbortUnblocksInFlightTurn` assert group death via
`kill(-pgid, 0)`. In this container PID 1 is `harness` itself, which does
not reap orphans, so killed grandchildren persist as zombies — and a
zombie still holds its PID and PGID, so `kill(-pgid, 0)` keeps succeeding
forever. The kill works; the liveness probe cannot observe it.

Direct evidence — the PGIDs named in the two failures contain nothing but
zombie (`Z`) `sleep` processes reparented to PID 1:

```
$ ps -o pid,ppid,stat,pgid,comm -p 4365,4366,4401,4402
  PID  PPID STAT  PGID COMMAND
 4365     1 Z     4363 sleep
 4366     1 Z     4363 sleep
 4401     1 Z     4399 sleep
 4402     1 Z     4399 sleep

$ ps -o pid,ppid,stat,comm -p 1
  PID  PPID STAT COMMAND
    1     0 Ssl  harness
```

Reduced to a 25-line program that mimics exactly what the bash tool does
(setpgid, group-kill, reap only the direct child):

```
$ go run /tmp/pgzombie.go
group 9006 still 'alive' after 3s -> test FAILS

$ ps -eo pid,ppid,stat,pgid,comm | awk '$4==9006'
 9008     1 Z     9006 sleep
 9009     1 Z     9006 sleep
```

`Setpgid` itself is fine here, so the failure is specifically the
unreapable-zombie artifact, not a process-group problem:

```
$ go run /tmp/pg.go
child pid=8882  getpgid=8882  err=<nil>
=> Setpgid WORKED (child leads its own group)
group kill syscall succeeded
```

They need a reaping init (tini/dumb-init) as PID 1; nothing in the branch
can affect them. They fail identically on pristine `main`, confirming this
branch does not cause them:

```
$ git stash -u && git checkout main && go test ./engine/ ./server/
--- FAIL: TestBashTimeoutKillsWholeProcessGroup (3.41s)
--- FAIL: TestBashAbortUnblocksInFlightTurn (3.17s)
FAIL	github.com/majorcontext/harness/engine	14.957s
--- FAIL: TestGarbageSessionIDsAreNotFound (0.01s)
FAIL	github.com/majorcontext/harness/server	14.817s
```

**Full suite on the declared toolchain.** Everything passes except those
two environment-blocked tests (one unrelated UI-timing flake in
`tools/monitor/e2e` appeared in this run and passes on retry, shown after):

```
$ GOTOOLCHAIN=go1.25.5 go test ./... -count=1
ok  	github.com/majorcontext/harness/cmd/harness	1.498s
ok  	github.com/majorcontext/harness/config	0.065s
ok  	github.com/majorcontext/harness/e2e	18.658s
--- FAIL: TestBashTimeoutKillsWholeProcessGroup (3.41s)
--- FAIL: TestBashAbortUnblocksInFlightTurn (3.17s)
FAIL	github.com/majorcontext/harness/engine	14.100s
ok  	github.com/majorcontext/harness/imageclamp	0.745s
ok  	github.com/majorcontext/harness/mcp	2.081s
ok  	github.com/majorcontext/harness/message	0.060s
ok  	github.com/majorcontext/harness/plugin	0.240s
ok  	github.com/majorcontext/harness/process	2.869s
ok  	github.com/majorcontext/harness/provider	0.004s
ok  	github.com/majorcontext/harness/provider/anthropic	0.152s
ok  	github.com/majorcontext/harness/provider/openai	0.085s
ok  	github.com/majorcontext/harness/provider/openaicompat	0.127s
ok  	github.com/majorcontext/harness/server	14.260s
ok  	github.com/majorcontext/harness/skill	0.049s
ok  	github.com/majorcontext/harness/tools/hub	0.093s
ok  	github.com/majorcontext/harness/tools/hub/e2e	3.352s
--- FAIL: TestRealEndToEnd (10.84s)
FAIL	github.com/majorcontext/harness/tools/monitor/e2e	10.852s
ok  	github.com/majorcontext/harness/typeid	0.006s
```

The monitor flake retried clean:

```
$ GOTOOLCHAIN=go1.25.5 go test ./tools/monitor/e2e/ -count=1
ok  	github.com/majorcontext/harness/tools/monitor/e2e	20.034s
```

**Clean run: the blocker is removable.** An earlier draft of this report
claimed these two tests "cannot pass in this container" and fell back to a
`-skip` run. That was wrong. The blocker is only the *absence of a reaping
PID 1*, and an unprivileged PID namespace supplies one: `unshare --user
--pid --fork` makes a forked process PID 1, and a ~20-line reaper
(`/tmp/reaper.py`) as that PID 1 reaps the orphans, so the group genuinely
disappears and `kill(-pgid, 0)` finally reports it.

The reduction that previously failed now passes under exactly that wrapper:

```
$ unshare --user --pid --fork --mount-proc python3 /tmp/reaper.py go run /tmp/pgzombie.go
group died -> test WOULD PASS
```

And so do the two tests themselves:

```
$ unshare --user --pid --fork --mount-proc python3 /tmp/reaper.py \
    env GOTOOLCHAIN=go1.25.5 go test ./engine/ \
    -run 'TestBashTimeoutKillsWholeProcessGroup|TestBashAbortUnblocksInFlightTurn' -count=1 -v
--- PASS: TestBashTimeoutKillsWholeProcessGroup (0.26s)
--- PASS: TestBashAbortUnblocksInFlightTurn (0.02s)
PASS
ok  	github.com/majorcontext/harness/engine	0.291s
```

**Full suite, no `-skip`, zero FAIL lines**, at this commit — the declared
toolchain plus a reaping PID 1, which is what the tests have always
assumed and what a normal CI container (tini/dumb-init) provides:

```
$ unshare --user --pid --fork --mount-proc python3 /tmp/reaper.py \
    env GOTOOLCHAIN=go1.25.5 go test ./... -count=1
ok  	github.com/majorcontext/harness/cmd/harness	1.598s
ok  	github.com/majorcontext/harness/config	0.077s
?   	github.com/majorcontext/harness/docs/scratch/probe/cacheprobe	[no test files]
?   	github.com/majorcontext/harness/docs/scratch/probe/sysbp	[no test files]
?   	github.com/majorcontext/harness/docs/scratch/probe/toolsize	[no test files]
ok  	github.com/majorcontext/harness/e2e	18.236s
ok  	github.com/majorcontext/harness/engine	7.649s
ok  	github.com/majorcontext/harness/imageclamp	0.600s
ok  	github.com/majorcontext/harness/mcp	2.064s
ok  	github.com/majorcontext/harness/message	0.060s
ok  	github.com/majorcontext/harness/plugin	0.249s
ok  	github.com/majorcontext/harness/process	1.517s
ok  	github.com/majorcontext/harness/provider	0.004s
ok  	github.com/majorcontext/harness/provider/anthropic	0.154s
ok  	github.com/majorcontext/harness/provider/openai	0.100s
ok  	github.com/majorcontext/harness/provider/openaicompat	0.095s
ok  	github.com/majorcontext/harness/server	14.062s
ok  	github.com/majorcontext/harness/skill	0.032s
ok  	github.com/majorcontext/harness/tools/hub	0.105s
ok  	github.com/majorcontext/harness/tools/hub/e2e	3.474s
?   	github.com/majorcontext/harness/tools/hub/e2e/hubverify	[no test files]
?   	github.com/majorcontext/harness/tools/monitor	[no test files]
ok  	github.com/majorcontext/harness/tools/monitor/e2e	19.281s
ok  	github.com/majorcontext/harness/typeid	0.011s
```

24 packages, every one `ok`, no FAIL line anywhere, no test excluded.

**Definitive final run**, captured as one transcript so the command line,
the commit under test, the working-tree state, the exit status and the
output are a single block rather than an output pasted after the fact.
`-count=1` bypasses Go's test cache, so nothing here is a cached result;
the bracketing timestamps (47s apart) match the summed package times.

```
$ date -u +%FT%TZ
2026-08-20T20:17:42Z

$ git -C /data/work/repo rev-parse HEAD
710b1002234d212d9abdecf7629848b8fb6445f1

$ git -C /data/work/repo status --porcelain | wc -l
0

$ unshare --user --pid --fork --mount-proc python3 /tmp/reaper.py env GOTOOLCHAIN=go1.25.5 go test ./... -count=1
ok  	github.com/majorcontext/harness/cmd/harness	1.777s
ok  	github.com/majorcontext/harness/config	0.080s
?   	github.com/majorcontext/harness/docs/scratch/probe/cacheprobe	[no test files]
?   	github.com/majorcontext/harness/docs/scratch/probe/sysbp	[no test files]
?   	github.com/majorcontext/harness/docs/scratch/probe/toolsize	[no test files]
ok  	github.com/majorcontext/harness/e2e	18.040s
ok  	github.com/majorcontext/harness/engine	8.125s
ok  	github.com/majorcontext/harness/imageclamp	0.500s
ok  	github.com/majorcontext/harness/mcp	2.063s
ok  	github.com/majorcontext/harness/message	0.064s
ok  	github.com/majorcontext/harness/plugin	0.260s
ok  	github.com/majorcontext/harness/process	1.528s
ok  	github.com/majorcontext/harness/provider	0.005s
ok  	github.com/majorcontext/harness/provider/anthropic	0.160s
ok  	github.com/majorcontext/harness/provider/openai	0.087s
ok  	github.com/majorcontext/harness/provider/openaicompat	0.096s
ok  	github.com/majorcontext/harness/server	19.013s
ok  	github.com/majorcontext/harness/skill	0.073s
ok  	github.com/majorcontext/harness/tools/hub	0.086s
ok  	github.com/majorcontext/harness/tools/hub/e2e	3.392s
?   	github.com/majorcontext/harness/tools/hub/e2e/hubverify	[no test files]
?   	github.com/majorcontext/harness/tools/monitor	[no test files]
ok  	github.com/majorcontext/harness/tools/monitor/e2e	20.799s
ok  	github.com/majorcontext/harness/typeid	0.005s

[exit status: 0]

$ date -u +%FT%TZ
2026-08-20T20:18:29Z
```

Exit status 0, 24 packages `ok`, zero FAIL lines, no test skipped, at
commit 710b1002234d212d9abdecf7629848b8fb6445f1 with a clean tree.

### Credentials

No credential value appears in any file on this branch. Verified:

```
$ grep -rInE '(mgt_|ogt_)[A-Za-z0-9]{16,}|sk-(ant|proj)[A-Za-z0-9_-]{8,}|Bearer +[A-Za-z0-9._-]{16,}' docs/scratch/
CLEAN: no credential values

$ git log main..HEAD -p -S"$BIFROST_API_KEY"     # value never printed
CLEAN: not introduced by any commit on this branch
```

The box's `.harness.json` (which does contain gateway tokens) is untracked
and was never staged:

```
$ git ls-files --error-unmatch .harness.json
Did you forget to 'git add'?
```

Note on `BIFROST_API_KEY` on this box: its value is the 16-character
placeholder used by the gatekeeper IP-bypass path, and the same literal
already appears in `server/thinking_live_test.go` on `main` as a fallback
default. It is not a secret, and this branch adds no occurrence of it.

## 5. Push status

`git push -u origin fx/mcp-lazy-probe` was attempted as the first action
after the skeleton commit, per the brief. The remote denied it:

```
$ git push -u origin fx/mcp-lazy-probe
remote: Permission to majorcontext/harness.git denied to andybons.
fatal: unable to access 'https://github.com/majorcontext/harness/': The requested URL returned error: 403
```

Work therefore continued as local commits on `fx/mcp-lazy-probe`.

---

## 6. Addendum: re-measurement under review findings F1-F4

An adversarial review returned REJECT with 17 findings. Four were
load-bearing against §3, and I accept all four. This addendum re-measures
the open question they left, and corrects the arithmetic.

**What the review found (accepted, not disputed):**

- **F1.** §3's "and then rides the cache from the next turn onward" was a
  generalization from a 2-turn experiment. The reviewer ran turn 3: the
  appended schema billed FRESH again (input=2085, read=8272 on turns 2 and
  3). Fresh-forever, not fresh-once. The original scenario D pinned the
  breakpoint to the end of the catalog, so an appended schema sat OUTSIDE
  the cached prefix permanently.
- **F2.** §2's sysbp probe tested tool REMOVAL only, never APPEND, under
  harness's real system-breakpoint arrangement. The reviewer measured
  append: creation=10039, read=0.
- **F3.** §3 rejected scenario B on PRICE CLASSES but computed break-even
  from RAW TOKEN COUNTS — two accounting systems in one argument. It also
  named only `transcode.go:207` (system block) and missed `:263` (last
  content block of the last message); harness has TWO ephemeral
  breakpoints.
- **F4.** 2,126 is a SINGLE SAMPLE (`cacheprobe:145`, tool 73 of 147), not
  an average. True per-tool mean is 87,959/147 = 598 tokens. §3 should
  never have implied otherwise.

### 6.1 Scenario E (C1): does a MOVING breakpoint rescue lazy loading?

F1 kills the FIXED-breakpoint design. The obvious repair is to move the
breakpoint onto the newly loaded schema, so it joins the cached prefix.
`docs/scratch/probe/movingbp/main.go`, four turns, one cold nonce:

```
$ go run ./docs/scratch/probe/movingbp -tools /tmp/mcpdump -gap 20s -nonce mv-run-1
catalog entries: 149 | schema1: mcp__linear__save_project_loaded1 (10039 B) | schema2: mcp__linear__get_attachment_loaded2 (300 B)

==================== SCENARIO E: moving BP, tools only (C1) ====================
--- t1: catalog only, BP on last catalog entry (expect COLD: read=0) ---
input=354 cache_creation=8293 cache_read=0 output=4 ttft=1.347s

--- t2: + schema1 appended, BP MOVED to schema1 (expect read=catalog, creation=schema1) ---
input=354 cache_creation=1778 cache_read=8293 output=4 ttft=0.939s

--- t3: BYTE-IDENTICAL to t2 -- DECISIVE: does read now cover catalog+schema1? ---
input=354 cache_creation=0 cache_read=10071 output=4 ttft=1.266s

--- t4: + schema2, BP moved to schema2 (expect read=catalog+schema1, creation=schema2) ---
input=354 cache_creation=0 cache_read=10150 output=4 ttft=0.864s
```

Independent cold repeat (`-nonce mv-run-2`), same numbers:

```
--- t1 --- input=354 cache_creation=8293 cache_read=0    output=1 ttft=1.157s
--- t2 --- input=354 cache_creation=1778 cache_read=8293 output=1 ttft=1.214s
--- t3 --- input=354 cache_creation=0    cache_read=10071 output=1 ttft=0.986s
--- t4 --- input=354 cache_creation=79   cache_read=10071 output=1 ttft=0.936s
```

**t3 is the decisive number: read=10071 = catalog 8293 + schema1 1778.**
The loaded schema IS inside the cached prefix from the turn after its
load. F1's fresh-forever behaviour is a property of the FIXED breakpoint,
not of lazy loading itself. t4 confirms it composes: a second load reads
back catalog+schema1 and writes only schema2 (10150 total either way; the
run-1/run-2 split of 10150+0 vs 10071+79 is only whether schema2's write
landed before or after the read boundary).

### 6.2 Scenario F (C2): what one load really costs under harness's shape

Tools precede system in Anthropic's prefix order, so growing the tools
array must re-create the system prefix behind it — F2's finding. Same
probe, with a system block carrying its own breakpoint
(`transcode.go:207` shape). Filler stands in for AGENTS.md at ~3.5k
tokens; real AGENTS.md is ~28.6k, so scale the system term by ~8x.

```
==================== SCENARIO F: + system block with its own BP (C2) ====================
system block: 14063 bytes (~3515 tokens) with its own ephemeral BP (harness transcode.go:207 shape)
--- t1: catalog + system BP (cold) ---
input=330 cache_creation=11318 cache_read=0 output=1 ttft=4.674s

--- t2: byte-identical (expect steady state: everything reads) ---
input=330 cache_creation=0 cache_read=11318 output=4 ttft=0.890s

--- t3: ONE mcp_load_tool -- schema1 appended, BP moved. THE COST OF A LOAD ---
input=330 cache_creation=4803 cache_read=8293 output=4 ttft=1.301s

--- t4: byte-identical to t3 -- does everything read back after the load? ---
input=330 cache_creation=0 cache_read=13096 output=4 ttft=0.767s
```

Run 2 reproduced all four turns exactly (11318/0, 0/11318, 4803/8293,
0/13096).

**The cost of one `mcp_load_tool` call, measured:** t3 creates 4,803
tokens while reading back only the 8,293-token catalog. Decomposed against
E t2's schema-alone write of 1,778:

- schema1 itself: 1,778 tokens
- system prefix re-write: 4,803 - 1,778 = **3,025 tokens** — exactly the
  system portion measured at t1 (11,318 - 8,293 = 3,025)

So F2 is confirmed quantitatively: a load re-creates the ENTIRE system
prefix, not just the schema. At this probe's ~3.5k system that is 3,025
tokens; **at AGENTS.md's real ~28.6k it is ~8x larger**, and that term —
not the schema — dominates the cost of a load. t4 then reads everything
back (13,096 = 8,293 + 1,778 + 3,025), so the cost is ONE-TIME per load,
not per turn.

### 6.3 Scenario G (C3): break-even, price-weighted

Redone consistently in one accounting system (read 0.10x, write 1.25x,
base 1.00x), using the true per-tool mean AND the single sample as a
range. 2,126 is NOT an average.

```
$ python3 (inline, see below)
=== steady-state per-turn cost, price-weighted ===
fat (today):        87959 x0.1 = 8795.9
thin +  0 loaded:   8293 x0.1 =    829.3   (saving   7966.6/turn)
thin +  1 loaded:   10071 x0.1 =   1007.1   (saving   7788.8/turn)
thin +  5 loaded:   11283 x0.1 =   1128.3   (saving   7667.6/turn)
thin + 10 loaded:   14273 x0.1 =   1427.3   (saving   7368.6/turn)

=== what ONE load costs, price-weighted (one-time) ===
schema write + system re-write: 4803 x1.25 = 6003.8

=== break-even ===
true mean 598 tok/tool: resident-tool cap before per-turn parity = 133.2 tools
   one load costs 4529 weighted; amortizes in 0.57 turns at 7967/turn saving
single sample 2,126 tok: resident-tool cap before per-turn parity = 37.5 tools
   one load costs 6439 weighted; amortizes in 0.81 turns at 7967/turn saving
```

The reviewer's price-weighted recomputation of the ORIGINAL (fixed-BP)
design gave 3.74 tools, and that is correct for that design, because there
the schema billed fresh EVERY turn. With the moving breakpoint the schema
is billed once as a write and then read, which changes the term from
recurring to one-time. The corrected figures:

- **Steady-state:** thin stays far cheaper. Even with 10 tools resident,
  1,427 vs 8,796 weighted tokens/turn — an 84% saving.
- **Resident-tool cap** (where per-turn cost reaches parity with fat):
  133 tools at the true 598-token mean, 37 at the 2,126 sample. Both are
  far above realistic session usage.
- **Amortization:** one load costs ~4.5k-6.4k weighted one-time and pays
  for itself in **under one turn** (0.57-0.81) against a ~7,967/turn
  saving. This is the number §3 asserted without measuring; it now holds,
  but only because of the moving breakpoint.

Caveat carried forward honestly: at AGENTS.md's real ~28.6k system, the
per-load system re-write grows ~8x to ~24k tokens (~30k weighted),
pushing amortization to roughly 3-4 turns per load. Still positive, but a
session that loads a tool every turn or two would spend most of the
benefit. That regime was not measured here.

### 6.4 Revised recommendation

**GO WITH CONDITIONS stands, but on different and narrower grounds than
§3 claimed, and §3's own reasoning is withdrawn.**

The original argument was wrong twice (F1 amortization never measured, F3
mixed accounting) and its design — fixed breakpoint at the end of the
catalog — is refuted outright by F1. What survives is a DIFFERENT design:

1. **The breakpoint must MOVE to the last loaded schema** on every load.
   A fixed catalog breakpoint makes every loaded schema bill fresh
   forever (F1). This is now condition #4, and it is load-bearing.
2. Conditions 1-3 from §3 (byte-stable catalog, append-only, never mutate
   on server loss) still hold.
3. **Budget the system re-write, not just the schema.** Each load
   re-creates the whole system prefix (§6.2); with real AGENTS.md that is
   the dominant term. An implementation should batch loads (one
   `mcp_load_tool` carrying several names) so the system prefix is
   re-written once per batch, not once per tool.
4. **Two unresolved blockers the reviewer raised, which this addendum does
   NOT clear and which gate implementation:**
   - The catalog's `{"type":"object"}` schema fails OPEN — every object
     validates, so a model can call an unloaded tool with invented
     arguments. Use a fail-closed placeholder such as
     `{"type":"object","not":{}}`.
   - The loaded-set is per-session mutable state that changes the wire
     request, colliding with the engine's stateless-transcoding
     invariant. It needs a durable record kind plus a `LoadSession` fold,
     or a resume silently reverts to the bare catalog. Neither is
     specified.

One-line conclusion: **moving-BP rescues lazy loading — steady-state reads
are clean (t3 read=10071 covers catalog+schema), and one load costs ~4,803
tokens one-time (1,778 schema + 3,025 system re-write), amortized in under
one turn at this system size.**

---

## 7. M1: real-scale, growing-history measurement

This is the measurement that decides Tier-1 #2, and it **reverses the
recommendation of §3 and §6**. Every prior probe held the message history
fixed at one short user turn and (in §6) used ~3.5k tokens of filler for
AGENTS.md. M1 replicates harness's real request shape end to end:

- **Tools:** thin catalog + moving BP on the last loaded schema (§6's
  revised design).
- **System:** the ACTUAL `AGENTS.md` (103,043 B) with its own BP
  (`transcode.go:207` shape).
- **Messages:** a GROWING history — a realistic user+assistant exchange
  appended every turn — with the `:263`-shape BP on the last content block
  of the last message, moving forward each turn as the transcoder does.

Two arms, separate cold nonces, six turns each, two independent cold runs
per arm. Probe: `docs/scratch/probe/m1/main.go`.

### 7.1 UNBATCHED arm (two load events, one schema each)

```
$ go run ./docs/scratch/probe/m1 -tools /tmp/mcpdump -agents AGENTS.md -arm unbatched -gap 15s -nonce m1u-1
system block: REAL AGENTS.md, 103043 bytes (~25760 tokens est) with its own BP (:207 shape)
tools: thin catalog 149 entries, moving BP on last loaded schema
messages: growing history, :263-shape BP on last content block of last message

--- turn 1: tools=149 (BP idx 148) history=1 msgs (~811 tok est) resident=0 ---
input=3 cache_creation=37032 cache_read=0 output=1 ttft=1.431s

--- turn 2: tools=149 (BP idx 148) history=3 msgs (~2025 tok est) resident=0 ---
input=3 cache_creation=1081 cache_read=37032 output=1 ttft=1.131s

### LOAD EVENT before turn 3: resident schemas now 1
--- turn 3: tools=150 (BP idx 149) history=5 msgs (~3238 tok est) resident=1 ---
input=3 cache_creation=32680 cache_read=8294 output=1 ttft=1.097s

--- turn 4: tools=150 (BP idx 149) history=7 msgs (~4452 tok est) resident=1 ---
input=3 cache_creation=1081 cache_read=40974 output=1 ttft=2.717s

### LOAD EVENT before turn 5: resident schemas now 2
--- turn 5: tools=151 (BP idx 150) history=9 msgs (~5665 tok est) resident=2 ---
input=3 cache_creation=33143 cache_read=10074 output=1 ttft=1.303s

--- turn 6: tools=151 (BP idx 150) history=11 msgs (~6879 tok est) resident=2 ---
input=3 cache_creation=1081 cache_read=43217 output=1 ttft=1.001s
```

Run 2 (`-nonce m1u-2`) reproduced all six turns to the token: 37032/0,
1081/37032, 32680/8294, 1081/40974, 33143/10074, 1081/43217.

### 7.2 BATCHED arm (one load event carrying four schemas)

```
$ go run ./docs/scratch/probe/m1 -tools /tmp/mcpdump -agents AGENTS.md -arm batched -gap 15s -nonce m1b-1
--- turn 1: tools=149 (BP idx 148) history=1 msgs (~811 tok est) resident=0 ---
input=3 cache_creation=37030 cache_read=0 output=1 ttft=1.316s
--- turn 2: tools=149 (BP idx 148) history=3 msgs (~2025 tok est) resident=0 ---
input=3 cache_creation=1081 cache_read=37030 output=1 ttft=0.750s
### LOAD EVENT before turn 3: resident schemas now 4
--- turn 3: tools=153 (BP idx 152) history=5 msgs (~3238 tok est) resident=4 ---
input=3 cache_creation=33586 cache_read=8293 output=1 ttft=1.197s
--- turn 4: tools=153 (BP idx 152) history=7 msgs (~4452 tok est) resident=4 ---
input=3 cache_creation=1081 cache_read=41879 output=1 ttft=0.753s
--- turn 5: tools=153 (BP idx 152) history=9 msgs (~5665 tok est) resident=4 ---
input=3 cache_creation=1081 cache_read=42960 output=1 ttft=0.865s
--- turn 6: tools=153 (BP idx 152) history=11 msgs (~6879 tok est) resident=4 ---
input=3 cache_creation=1081 cache_read=44041 output=1 ttft=0.789s
```

Run 2 (`-nonce m1b-2`) reproduced all six turns to the token.

### 7.3 What the numbers say

**The moving BP still works, and post-load turns do read the grown history
back.** Turn 4 reads 40,974 (unbatched) / 41,879 (batched) — catalog +
system + loaded schemas + the whole history. §6.1's mechanism survives
contact with a real request shape.

**A load event invalidates EVERYTHING behind the tools array.** At the load
turn the read collapses from 37,032 to 8,294 — the catalog alone. The
system block and the entire message history are re-created:

```
pre-load steady read      37032
read at load turn          8294  <- catalog ONLY
lost from cache           28738
creation at load turn     32680
  system+history rewrite  ~28739   (t1 create 37032 - catalog 8293)
  schema1 itself            1778   (from movingbp E t2)
  => the system/history term is 94.6% of the load cost
```

**The load cost is essentially independent of how many schemas are
loaded.** One schema costs 32,680; four cost 33,586 — 906 tokens for three
extra schemas. The payload is noise; the invalidation is the cost.

```
unbatched: 2 events = 65,823 for 2 schemas = 32,912/schema
batched:   1 event  = 33,586 for 4 schemas =  8,396/schema
```

That is the strongest possible argument for batching — and also the
finding that sinks the design, because it means the cost is per LOAD
EVENT, and a load event is exactly what a lazy loader does whenever the
model needs an unforeseen tool.

### 7.4 Price-weighted decision arithmetic (read .10 / write 1.25)

```
=== price-weighted per-turn steady state ===
fat (today), steady:        13021.1
thin, 0 resident:            5054.5   saving   7966.6/turn
thin, 4 resident (t6):       5755.4   saving   7265.7/turn

=== amortization: turns to repay ONE load event ===
unbatched (1 schema): cost  40850.0 weighted ->  5.62 turns  ( 5.62 turns/schema)
batched (4 schemas):  cost  41982.5 weighted ->  5.78 turns  ( 1.44 turns/schema)

=== load-cadence break-even vs fat ===
unbatched: thin loses if a load event happens more often than every 5.62 turns
batched-4: thin loses if a load event happens more often than every 5.78 turns
```

Applying the reviewer's decision rule literally: the batched arm's
amortized cost per LOAD EVENT is **5.78 turns**, which is NOT comfortably
under the ~4.71-turn threshold — it is above it. At real scale with a
growing history, the per-event repay got *worse*, not better, than the
reviewer's AGENTS.md-only figure, because the history is now also being
re-created on every load.

The per-SCHEMA figure (1.44 turns) is favourable, and it is the number a
proponent would quote. It should not decide this: schemas are not loaded
on a schedule, they are loaded when the model discovers it needs one, and
each such discovery is a full-price event regardless of how many schemas
it happens to carry. Batching only helps to the extent the loader can
predict a batch, which is precisely what a lazy loader cannot do.

### 7.5 Conclusion: DROP Tier-1 #2

**Recommendation reversed. I withdraw the GO WITH CONDITIONS of §3 and
§6.4.**

The steady-state saving is real (7,266-7,967 weighted tokens/turn, ~56%
per-turn). But it is only realizable by a session that almost never loads
a tool — and a session that almost never loads a tool did not need lazy
loading. The design pays ~41-42k weighted tokens per load event, needs
~5.8 turns to repay it, and a realistic agent session discovers a needed
tool far more often than every six turns.

Three independent reasons to drop, in order of strength:

1. **The cost is per-EVENT and event-frequency is unpredictable**
   (§7.3). 94.6% of a load is invalidation of system+history, not the
   schema. No amount of catalog engineering touches that term.
2. **Real scale made it worse, not better.** §6 measured 4,803/load at
   ~3.5k system and projected 3-4 turns; at real AGENTS.md with a growing
   history it is ~32.7k/load and 5.78 turns. My §6 extrapolation was
   conservative in the schema term and missed the history term entirely.
3. **Two structural blockers from the review remain unresolved** and are
   independent of any cache measurement: the fail-open
   `{"type":"object"}` catalog schema, and the loaded-set as unpersisted
   per-session state colliding with stateless transcoding.

What would change this verdict: if harness reordered its prefix so tools
come AFTER system and history (a wire-shape change, not a config knob), a
tools-array append would stop invalidating the expensive part. That is a
much larger change than Tier-1 #2 contemplates, and it is the only version
of this idea worth revisiting.

One-line conclusion: **DROP Tier-1 #2 — at real scale with a growing
history, one load event costs ~32.7k tokens (94.6% of it system+history
re-creation, near-constant regardless of schema count) and takes 5.78
turns to repay, above the ~4.7-turn cadence threshold; batching helps
per-schema (1.44 turns) but cannot fix a per-event cost a lazy loader
cannot schedule.**
