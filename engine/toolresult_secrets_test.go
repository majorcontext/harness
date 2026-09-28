package engine

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
)

// TestMaskSecretsDoesNotDeleteAdjacentContent proves an unbounded value
// pattern (\S+) does not delete everything from a key-shaped match to
// the next whitespace: \S+ does not stop at "&", "?", ",", or any other
// structural delimiter, only at whitespace, which on a "token=<huge
// blob>" line can lose nearly the entire line. This constructs the same
// shape (a URL-like single line with a token mid-string followed by
// unrelated, legitimate data with no separating whitespace) and asserts
// the loss is bounded to roughly the masked value's own length, never
// anywhere close to the whole remainder.
func TestMaskSecretsDoesNotDeleteAdjacentContent(t *testing.T) {
	t.Parallel()
	before := "https://example.com/callback?state=xyz&"
	secret := strings.Repeat("A", 6_000) // no whitespace near it; far above the {8,1000} cap
	after := "&next=" + strings.Repeat("legituserdata", 50) + "&done=1"
	in := before + "token=" + secret + after

	got := maskSecrets(in)

	if !strings.HasPrefix(got, before+"token=") {
		t.Fatalf("prefix up to and including the key/separator was altered:\n got: %s\nwant prefix: %s", got[:min(80, len(got))], before+"token=")
	}
	if !strings.HasSuffix(got, after) {
		t.Fatalf("adjacent, unrelated content after the secret was NOT preserved: got suffix %q, want it to end with %q",
			got[max(0, len(got)-len(after)-20):], after)
	}
	// The masked SPAN itself must be small (per the {8,1000} cap, which
	// exists so a long SECRET masks more completely; the character class
	// alone is what protects adjacent content): the vast
	// majority of the "A" run must still be present, UNMASKED, in the
	// output — only the first (up to) 1000 of them are inside the match.
	// (Direct length subtraction is not a safe measure: with the bulk of
	// the "A" run surviving, len(got) is barely smaller than len(in),
	// which is exactly the point — so this counts surviving "A" runs
	// directly instead.)
	longestARun := 0
	current := 0
	for _, r := range got {
		if r == 'A' {
			current++
			if current > longestARun {
				longestARun = current
			}
		} else {
			current = 0
		}
	}
	if longestARun < len(secret)-1050 {
		t.Errorf("masking destroyed the bulk of a large legitimate value: longest surviving run of \"A\" = %d, want close to the original %d (only ~1000 chars should ever be inside the match)",
			longestARun, len(secret))
	}
}

// TestMaskSecretsValueClassStopsAtDelimiters is the regression guard for
// the bounded CHARACTER CLASS, not the length cap. A mutant that reverts
// secretValueClass to `\S` (keeping the
// {8,1000} cap) still bleeds across `&`-delimited URL parameters — it eats
// "SECRETVALUE&Expires=...&Signature=..." as one "value" — while the whole
// rest of the suite stays green (the adjacent-content test above measures
// loss in the hundreds of bytes, inside the cap's slack). This test pins
// the class itself: masking must stop at the first structural delimiter,
// so the parameters AFTER the secret survive byte-for-byte.
func TestMaskSecretsValueClassStopsAtDelimiters(t *testing.T) {
	t.Parallel()
	in := `GET "https://bucket.s3.amazonaws.com/obj?access_key=AKIAEXAMPLE12345&Expires=1735689600&Signature=abcdefghijklmnop" -> 200`
	got := maskSecrets(in)

	if !strings.Contains(got, "access_key=***") {
		t.Fatalf("the secret value itself was not masked: %q", got)
	}
	if !strings.Contains(got, "&Expires=1735689600&Signature=abcdefghijklmnop") {
		t.Errorf("masking bled past the value's closing delimiter and destroyed adjacent URL parameters (the \\S-class regression):\n got: %q", got)
	}
	// A long secret must be masked IN FULL up to the cap: the env/YAML value
	// bound is {8,1000}, high enough that the cap rarely limits how much of
	// a LONG SECRET is masked, while the character class alone is what
	// protects adjacent content.
	longSecret := strings.Repeat("s", 400)
	gotLong := maskSecrets("token=" + longSecret + " trailing-context")
	if strings.Contains(gotLong, "ssssssss") {
		t.Errorf("a 400-char secret value survived masking (want the whole delimiter-free run masked): %q", gotLong[:min(120, len(gotLong))])
	}
	if !strings.HasSuffix(gotLong, " trailing-context") {
		t.Errorf("content beyond the secret's whitespace delimiter was destroyed: %q", gotLong)
	}
}

// TestMaskSecretsMultilineJSONNotBypassedByLineSplitting proves the
// per-line optimization is NOT equivalent to a whole-text pass: in RE2,
// `[^"]`, `[^']`, and `\s` all match `\n`, so the quoted-JSON separator
// (`\s*:\s*`) and the Bearer
// prefix's whitespace CAN span a newline — a pretty-printed
//
//	"api_key":
//	  "secretvalue123456"
//
// matches (and is masked) when maskSecretsSpan runs over the WHOLE text,
// but `strings.SplitAfter` fed the key line and the value line to
// maskSecretsSpan as two INDEPENDENT spans: neither alone matches any
// alternative, so the secret reached disk and the inline preview
// completely unmasked — the two code paths (single-span fallback vs.
// per-line) disagreed about which bytes are secret.
func TestMaskSecretsMultilineJSONNotBypassedByLineSplitting(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, wantContains, wantValueGone string
	}{
		{
			name:          "key then value on next line",
			in:            "{\n  \"api_key\":\n  \"secretvalue123456\"\n}\n",
			wantContains:  `"api_key":`,
			wantValueGone: "secretvalue123456",
		},
		{
			name:          "bearer prefix then token on next line",
			in:            "Authorization:\nBearer eyJhbGciOiJIUzI1NiJ9.payloadvalue.sig123456\n",
			wantContains:  "Authorization:",
			wantValueGone: "eyJhbGciOiJIUzI1NiJ9.payloadvalue.sig123456",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := maskSecrets(tc.in)
			if strings.Contains(got, tc.wantValueGone) {
				t.Errorf("secret value survived masking (line-split bypass): %q -> %q", tc.in, got)
			}
			if !strings.Contains(got, tc.wantContains) {
				t.Errorf("masked output missing expected key text %q: got %q", tc.wantContains, got)
			}
			if !strings.Contains(got, "***") {
				t.Errorf("no masking marker present at all: %q", got)
			}
		})
	}
}

// TestMaskSecretsQuotedJSON is the red test for the
// quoted-JSON "key": "value" shape, both with and without whitespace
// around the colon.
func TestMaskSecretsQuotedJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, wantContains, wantValueGone string
	}{
		{
			name:          "spaced",
			in:            `{"user":"alice","token": "eyJhbGciOiJIUzI1NiJ9.somepayloadvalue.sig123456","ok":true}`,
			wantContains:  `"token": "***"`,
			wantValueGone: "eyJhbGciOiJIUzI1NiJ9.somepayloadvalue.sig123456",
		},
		{
			name:          "unspaced",
			in:            `{"api_key":"AKIAABCDEFGHIJKLMNOP","region":"us-east-1"}`,
			wantContains:  `"api_key":"***"`,
			wantValueGone: "AKIAABCDEFGHIJKLMNOP",
		},
		{
			name:          "compound key",
			in:            `{"AWS_SECRET_ACCESS_KEY":"wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY"}`,
			wantContains:  `"AWS_SECRET_ACCESS_KEY":"***"`,
			wantValueGone: "wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := maskSecrets(tc.in)
			if !strings.Contains(got, tc.wantContains) {
				t.Errorf("masked output missing %q:\n%s", tc.wantContains, got)
			}
			if strings.Contains(got, tc.wantValueGone) {
				t.Errorf("secret value survived masking:\n%s", got)
			}
			// Unrelated fields must survive byte-identical.
			if tc.name == "spaced" && (!strings.Contains(got, `"user":"alice"`) || !strings.Contains(got, `"ok":true`)) {
				t.Errorf("unrelated JSON fields were altered:\n%s", got)
			}
		})
	}
}

// TestMaskSecretsSpaceYAML is the red test for the
// space-YAML "key: value" shape.
func TestMaskSecretsSpaceYAML(t *testing.T) {
	t.Parallel()
	in := "database:\n  host: localhost\n  password: hunter2hunter2hunter2\napi_key: sk-ANTAPI03abcdefghijklmnop\n"
	got := maskSecrets(in)
	if strings.Contains(got, "hunter2hunter2hunter2") {
		t.Errorf("YAML password value survived masking:\n%s", got)
	}
	if strings.Contains(got, "sk-ANTAPI03abcdefghijklmnop") {
		t.Errorf("YAML api_key value survived masking:\n%s", got)
	}
	if !strings.Contains(got, "password: ***") {
		t.Errorf("YAML password shape not masked in the expected form:\n%s", got)
	}
	if !strings.Contains(got, "host: localhost") {
		t.Errorf("unrelated YAML content was altered:\n%s", got)
	}
}

// TestMaskSecretsQuotedEnvValue is the red test proving
// `export TOKEN="secretvalue123"` — an unquoted key with a QUOTED value,
// an extremely common shell/env-dump shape — does not slip through
// entirely unmasked. The env/YAML alternative requires its value class
// immediately after the separator, but the next byte there is `"` (not in
// secretValueClass), so it never matches; the JSON alternative requires a
// QUOTED key, which a bare `TOKEN` lacks. Both shapes miss it on their
// own.
func TestMaskSecretsQuotedEnvValue(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, in, wantMasked, wantValueGone string }{
		{"double-quoted-equals", `export TOKEN="secretvalue123456"`, `TOKEN="***"`, "secretvalue123456"},
		{"single-quoted-equals", `export TOKEN='secretvalue123456'`, `TOKEN='***'`, "secretvalue123456"},
		{"quoted-yaml", "api_key: \"sk-ANTAPI03abcdefghijklmnop\"", `api_key: "***"`, "sk-ANTAPI03abcdefghijklmnop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := maskSecrets(tc.in)
			if strings.Contains(got, tc.wantValueGone) {
				t.Errorf("secret value survived masking: %q -> %q", tc.in, got)
			}
			if !strings.Contains(got, tc.wantMasked) {
				t.Errorf("masked output missing expected shape %q: got %q", tc.wantMasked, got)
			}
		})
	}
}

// TestMaskSecretsAuthorizationBearer is the red test for
// the Authorization: Bearer <token> header shape.
func TestMaskSecretsAuthorizationBearer(t *testing.T) {
	t.Parallel()
	in := "GET /api/v1/widgets HTTP/1.1\nHost: example.com\nAuthorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.somepayload.signaturevalue\nAccept: application/json\n"
	got := maskSecrets(in)
	if strings.Contains(got, "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.somepayload.signaturevalue") {
		t.Errorf("bearer token survived masking:\n%s", got)
	}
	if !strings.Contains(got, "Authorization: Bearer ***") {
		t.Errorf("Authorization header not masked in the expected form:\n%s", got)
	}
	if !strings.Contains(got, "Host: example.com") || !strings.Contains(got, "Accept: application/json") {
		t.Errorf("unrelated headers were altered:\n%s", got)
	}
}

// TestMaskSecretsCodeCorpus is the red test proving realistic
// source snippets across a few languages survive masking BYTE
// IDENTICAL. The named risk is Go's `:=` (token:=lexer.Next()
// becoming "token:*** if..."), but the corpus covers the same shape in a
// few other common forms too.
func TestMaskSecretsCodeCorpus(t *testing.T) {
	t.Parallel()
	cases := []string{
		// The named risk: Go short variable declaration.
		"token := lexer.Next()",
		"token:=lexer.Next()", // the same shape with no spaces around :=
		"secret, err := loadSecret(path)",
		"password, ok := lookupPassword(ctx, userID)",
		"apiKey := os.Getenv(\"API_KEY\")",
		// Ordinary Go comparisons/field access — spaced, no adjacency to
		// a "=" or ":" separator.
		"if password == \"\" {\n\treturn errEmptyPassword\n}",
		"func GetToken() string {\n\treturn s.token\n}",
		"type Config struct {\n\tAPIKey string `json:\"api_key,omitempty\"`\n}",
		// Python/JS, formatter-produced (spaced assignment).
		"password = input(\"Enter password: \")",
		"const token = await getToken();",
		"let apiKey = process.env.API_KEY;",
		// A short TS type annotation (value too short to satisfy the
		// {8,200} floor either way).
		"interface Config {\n  password: string;\n}",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			got := maskSecrets(in)
			if got != in {
				t.Errorf("code snippet was altered by masking:\n  in: %s\n got: %s", in, got)
			}
		})
	}
}

// TestMaskSecretsPreview is the integration red test proving the
// PREVIEW half of a retained result (the bytes that go straight into the
// provider request, inline) is masked exactly like the sidecar file:
// masking only what reaches disk would let a secret sitting within the
// first ToolResultInlineBytes reach the model in cleartext regardless of
// masking existing at all.
func TestMaskSecretsPreview(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	secretValue := "AKIAABCDEFGHIJKLMNOP"
	text := "AWS_SECRET_ACCESS_KEY=" + secretValue + "\n" + linesText(3000)

	prov := oneToolTurnProvider("bigtool")
	cfg := retainCfg(dir, prov, 200, 0) // small enough that the secret line lands INSIDE the preview
	cfg.Tools = []Tool{bigOutputTool("bigtool", text)}

	_, tr := runOneToolTurn(t, cfg, prov, "bigtool")

	if len(tr.Content) < 2 {
		t.Fatalf("expected header+preview content, got %d parts", len(tr.Content))
	}
	header := tr.Content[0].(*message.Text).Text
	preview := tr.Content[1].(*message.Text).Text
	if strings.Contains(header, secretValue) {
		t.Errorf("secret value leaked into the header:\n%s", header)
	}
	if strings.Contains(preview, secretValue) {
		t.Errorf("secret value went inline to the model UNMASKED in the preview:\n%s", preview)
	}
	if !strings.Contains(preview, "AWS_SECRET_ACCESS_KEY=***") {
		t.Errorf("preview does not carry the masked form:\n%s", preview)
	}
}

// TestToolResultMetaBytesMatchesOnDiskLength is the accounting red test
// proving meta.Bytes (and the header's/read_tool_result's "bytes=%d")
// describes the length of what is ACTUALLY on disk — post-mask — not the
// original pre-mask length. Reporting the original length would leave
// the header/read_tool_result advertising a size the sidecar file does
// not have, since a masked value shrinks the text (every secret does:
// "***" is shorter than almost anything it replaces).
func TestToolResultMetaBytesMatchesOnDiskLength(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	secretValue := strings.Repeat("A", 100) // a value substantially longer than "***"
	text := "TOKEN=" + secretValue + "\n" + linesText(3000)

	prov := oneToolTurnProvider("bigtool")
	cfg := retainCfg(dir, prov, 200, 0)
	cfg.Tools = []Tool{bigOutputTool("bigtool", text)}

	s, _ := runOneToolTurn(t, cfg, prov, "bigtool")

	meta, ok := s.lookupToolResult("trh_1")
	if !ok {
		t.Fatal("trh_1 not registered")
	}
	onDisk, err := os.ReadFile(s.toolResultPath("trh_1"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Bytes != len(onDisk) {
		t.Errorf("meta.Bytes = %d, on-disk file is %d bytes — meta.Bytes must describe what is ACTUALLY on disk (post-mask), not the pre-mask original", meta.Bytes, len(onDisk))
	}
	if meta.Bytes == len(text) {
		t.Errorf("meta.Bytes (%d) equals the ORIGINAL pre-mask length (%d) — masking should have shrunk it (the secret's ~100-byte value became \"***\")", meta.Bytes, len(text))
	}
}

// TestMaskSecretsPerformance measures masking cost against three shapes
// of 4.4 MB input:
//
//   - "no_candidates": ordinary multi-line output with no secret-shaped
//     keyword anywhere — the common case. Must be near-instant: this is
//     the containsSecretCandidate whole-text fast-reject alone.
//   - "sparse_realistic": ordinary multi-line output with a FEW
//     secret-shaped lines scattered through it (roughly one per 20 KB) —
//     representative of a real env dump or build log. This is the case
//     the 100ms/4MB target is actually about, and the one the
//     line-level pre-filter (see maskSecrets's doc comment) is built for.
//   - "single_huge_line": the pathological case — one multi-megabyte
//     line (no newlines at all) that DOES contain a secret. The line
//     pre-filter cannot help here (there is only one "line"), so this
//     falls back to a single full-text regex scan — documented as a
//     known-slower residual, not a target for the 100ms ceiling.
//
// An unbounded (\S+-based) masker has been observed to take several
// hundred milliseconds over 4.4 MB (one unbounded pattern, one pass);
// the line-level pre-filter exists to keep the common cases far below
// that.
// maskSecretsPerfInput builds one performance test corpus: ~4.4MB of
// ordinary log lines with a "secret" line inserted every secretEvery lines
// (0 disables insertion entirely — the no-candidate-lines fast-reject
// case). Shared between TestMaskSecretsPerformance and
// BenchmarkMaskSecrets so both measure the identical corpora.
func maskSecretsPerfInput(secretEvery int) string {
	var b strings.Builder
	line := "some ordinary log line with a bit of prose in it, id=" + strings.Repeat("x", 20) + "\n"
	secretLine := "AWS_SECRET_ACCESS_KEY=AKIAABCDEFGHIJKLMNOPQRSTUVWXYZ1234\n"
	n := 0
	for b.Len() < 4_400_000 {
		b.WriteString(line)
		n++
		if secretEvery > 0 && n%secretEvery == 0 {
			b.WriteString(secretLine)
		}
	}
	return b.String()
}

// TestMaskSecretsPerformance is a coarse regression GUARD, not a precise
// timing assertion — see BenchmarkMaskSecrets below for the actual
// perf-tracking tool (run with `go test -bench MaskSecrets -run ^$`,
// outside -race, for numbers worth comparing across commits).
//
// The ceilings here were tightened once, to catch a real regression
// early, and that tight 1s ceiling on sparse_realistic became a
// standing CI flake: it failed on CI three separate times
// (https://github.com/majorcontext/harness/actions/runs/32659723380/job/97243923949,
// https://github.com/majorcontext/harness/actions/runs/32660777608/job/97246510121,
// and one earlier failure predating this PR's branch, on main's own CI
// history) at 1.02-1.06s — a hair over the ceiling — while never once
// failing across 9+ isolated local runs (both `-count=N` repeats and a
// dedicated same-machine A/B against origin/main), which consistently
// measured 0.62-0.66s. The pattern is exactly what CPU contention from
// OTHER packages/goroutines running concurrently in the same `go test
// ./...` invocation looks like — this test's own timing is sound, the
// ABSOLUTE ceiling was just too tight to survive a loaded CI runner. No
// same-process relative baseline was used instead (e.g. comparing
// against no_candidates): the fast-reject path no_candidates takes is
// cheap enough (~20ms, dominated by a trivial per-line scan, not the
// regex engine) that it stays fast even under the exact contention that
// slows sparse_realistic down — a ratio against it would not track the
// same load signal at all. Ceilings below are instead sized as roughly
// 10x the documented worst-case-under-load baseline: generous enough to
// absorb realistic CI noise, but still tight enough to catch an actual
// order-of-magnitude algorithmic regression (a real hang, or a change
// that reintroduces the O(n²) shape earlier rounds fixed).
func TestMaskSecretsPerformance(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		ceiling  time.Duration
		skipRace bool
	}{
		// Never observed above ~20ms (the fast-reject path barely touches
		// the regex engine at all) — 1s is already >>10x its worst
		// observed cost, so it's left as-is.
		{"no_candidates", maskSecretsPerfInput(0), 1 * time.Second, false},
		// Documented worst-case-under-load: 1.06s (see the three CI runs
		// cited in this function's doc comment). 10s is ~10x that, and
		// ~15x the clean-isolation baseline (~0.66s).
		{"sparse_realistic", maskSecretsPerfInput(300), 10 * time.Second, false}, // ~1 secret line per ~300 ordinary lines
		// This is the one case with no line-level fast-reject (see
		// maskSecrets's doc comment), so it is the case most worth a
		// ceiling. 20s is ~10x the observed plain-mode cost (~1.9s),
		// the same headroom multiple as sparse_realistic. Skipped
		// under -race: the race detector's instrumentation overhead on
		// this regex-heavy path dominates the measurement, so the
		// ceiling would guard instrumentation cost, not maskSecrets.
		// BenchmarkMaskSecrets tracks real timing instead.
		{"single_huge_line", "TOKEN=" + strings.Repeat("y", 4_400_000), 20 * time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.skipRace && raceEnabled {
				t.Skip("ceiling measures race instrumentation, not maskSecrets; see BenchmarkMaskSecrets")
			}

			start := time.Now()
			out := maskSecrets(tc.input)
			elapsed := time.Since(start)

			t.Logf("maskSecrets(%s): %d bytes in %s (%.2f MB/s)", tc.name, len(tc.input), elapsed, float64(len(tc.input))/1e6/elapsed.Seconds())
			if elapsed > tc.ceiling {
				t.Errorf("maskSecrets(%s) took %s over %d bytes — want <= %s", tc.name, elapsed, len(tc.input), tc.ceiling)
			}
			if len(out) == 0 {
				t.Fatal("masked output is empty")
			}
		})
	}
}

// BenchmarkMaskSecrets is the actual perf-tracking tool for maskSecrets —
// run it explicitly (`go test ./engine/ -bench MaskSecrets -run ^$`,
// outside -race, whose instrumentation overhead swamps the real cost) to
// compare timings across commits. TestMaskSecretsPerformance above is
// deliberately a much coarser guard against an egregious regression, not
// a substitute for this: see its own doc comment for why a tight absolute
// ceiling on the same measurement was a standing CI flake.
func BenchmarkMaskSecrets(b *testing.B) {
	cases := []struct {
		name  string
		input string
	}{
		{"no_candidates", maskSecretsPerfInput(0)},
		{"sparse_realistic", maskSecretsPerfInput(300)},
		{"single_huge_line", "TOKEN=" + strings.Repeat("y", 4_400_000)},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(tc.input)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				maskSecrets(tc.input)
			}
		})
	}
}
