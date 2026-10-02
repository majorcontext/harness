#!/usr/bin/env bash
# Measures the statement coverage that the contract suite (e2e TestContract*)
# reaches in the real harness binary. The e2e build is instrumented when
# HARNESS_E2E_COVER=1; each serve process writes counters to GOCOVERDIR when it
# exits on SIGTERM. A process that is killed writes none.
#
# Environment:
#   CONTRACT_COVER_DIR  keep the counters and profile in this directory
#   CONTRACT_COVER_TOP  number of uncovered functions to print (default 25)
#   GITHUB_STEP_SUMMARY when set, the per-package table is appended to it
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
top=${CONTRACT_COVER_TOP:-25}
work=${CONTRACT_COVER_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/harness-contract-cover.XXXXXX")}
if [ -z "${CONTRACT_COVER_DIR:-}" ]; then
	trap 'rm -rf "$work"' EXIT
fi
covdata=$work/covdata
rm -rf "$covdata"
mkdir -p "$covdata"

cd "$root"
HARNESS_E2E_COVER=1 GOCOVERDIR=$covdata go test -race -count=1 ./e2e/ -run TestContract

if [ -z "$(ls -A "$covdata")" ]; then
	echo "contract-coverage: no counters written to $covdata" >&2
	exit 1
fi

echo
echo "== statement coverage by package =="
go tool covdata percent -i="$covdata" | tee "$work/percent.txt"

go tool covdata textfmt -i="$covdata" -o="$work/profile.txt"
go tool cover -func="$work/profile.txt" >"$work/func.txt"
total=$(awk '/^total:/ {print $NF}' "$work/func.txt")
echo
echo "total: $total"

echo
echo "== top $top functions by uncovered statements =="
awk -v top="$top" '
	FNR == NR {
		if ($1 ~ /^total:/) next
		split($1, loc, ":")
		file = loc[1]
		line = loc[2] + 0
		n = ++count[file]
		start[file, n] = line
		name[file, n] = $2
		next
	}
	/^mode:/ { next }
	{
		split($1, a, ":")
		file = a[1]
		split(a[2], b, ",")
		split(b[1], c, ".")
		line = c[1] + 0
		stmts = $2 + 0
		hits = $3 + 0
		best = 0
		for (i = 1; i <= count[file]; i++) {
			if (start[file, i] <= line) best = i
		}
		if (best == 0) next
		key = file ":" name[file, best]
		total_stmts[key] += stmts
		if (hits == 0) missed[key] += stmts
	}
	END {
		for (key in missed) printf "%d\t%d\t%s\n", missed[key], total_stmts[key], key
	}
' "$work/func.txt" "$work/profile.txt" | sort -t "$(printf '\t')" -k1,1nr -k3,3 |
	sed 's#github.com/majorcontext/harness/##' |
	awk -F '\t' -v top="$top" 'BEGIN { printf "%8s %8s  %s\n", "missed", "stmts", "function" } NR <= top { printf "%8d %8d  %s\n", $1, $2, $3 }'

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
	{
		echo "### Contract suite coverage"
		echo
		echo "Total: $total"
		echo
		echo "| Package | Statements covered |"
		echo "| --- | --- |"
		sed -E 's#^[[:space:]]*github.com/majorcontext/harness/?##; s#[[:space:]]+coverage:[[:space:]]+# | #; s#[[:space:]]+of statements##' "$work/percent.txt" |
			sed -E 's#^\| #(root) | #; s#^#| #; s#$# |#'
	} >>"$GITHUB_STEP_SUMMARY"
fi
