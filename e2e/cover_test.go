package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

const coverModule = "github.com/majorcontext/harness"

var coverRowPattern = regexp.MustCompile(`^\s*(\S+)\s+coverage:\s+([0-9.]+)% of statements`)

type coverRow struct{ pkg, percent string }

func coverRows(percent string) []coverRow {
	var rows []coverRow
	for _, line := range strings.Split(percent, "\n") {
		m := coverRowPattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		pkg := strings.TrimPrefix(strings.TrimPrefix(m[1], coverModule), "/")
		if pkg == "" {
			pkg = "(root)"
		}
		rows = append(rows, coverRow{pkg, m[2] + "%"})
	}
	return rows
}

func coverMarkdown(rows []coverRow, total string) string {
	var b strings.Builder
	b.WriteString("### Contract suite coverage\n\n")
	fmt.Fprintf(&b, "Total: %s\n\n| Package | Statements covered |\n| --- | --- |\n", total)
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %s |\n", r.pkg, r.percent)
	}
	return b.String()
}

// startCover points GOCOVERDIR at a fresh directory when HARNESS_E2E_COVER=1.
// The returned func prints the per-package coverage that the serve processes
// wrote, and appends a Markdown table to $GITHUB_STEP_SUMMARY. A process that
// is killed writes no counters.
func startCover() (finish func() error, err error) {
	if os.Getenv("HARNESS_E2E_COVER") != "1" {
		return func() error { return nil }, nil
	}
	dir, err := os.MkdirTemp("", "harness-e2e-cover")
	if err != nil {
		return nil, err
	}
	if err := os.Setenv("GOCOVERDIR", dir); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return func() error {
		defer func() { _ = os.RemoveAll(dir) }()
		return reportCover(dir)
	}, nil
}

func reportCover(dir string) error {
	out, err := exec.Command("go", "tool", "covdata", "percent", "-i="+dir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("covdata percent: %v\n%s", err, out)
	}
	rows := coverRows(string(out))
	if len(rows) == 0 {
		return fmt.Errorf("no coverage counters written to %s", dir)
	}
	total, err := coverTotal(dir)
	if err != nil {
		return err
	}
	fmt.Printf("\n== statement coverage by package ==\n%s\ntotal: %s\n", out, total)
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(coverMarkdown(rows, total)); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func coverTotal(dir string) (string, error) {
	profile := dir + "/profile.txt"
	if out, err := exec.Command("go", "tool", "covdata", "textfmt", "-i="+dir, "-o="+profile).CombinedOutput(); err != nil {
		return "", fmt.Errorf("covdata textfmt: %v\n%s", err, out)
	}
	out, err := exec.Command("go", "tool", "cover", "-func="+profile).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("cover -func: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) == 0 || fields[0] != "total:" {
		return "", fmt.Errorf("cover -func has no total line: %q", lines[len(lines)-1])
	}
	return fields[len(fields)-1], nil
}

func TestCoverRows(t *testing.T) {
	skipShort(t)
	in := "\tgithub.com/majorcontext/harness\t\tcoverage: 12.5% of statements\n" +
		"\tgithub.com/majorcontext/harness/provider/anthropic\t\tcoverage: 68.7% of statements\n"
	want := "### Contract suite coverage\n\nTotal: 38.2%\n\n| Package | Statements covered |\n| --- | --- |\n" +
		"| (root) | 12.5% |\n| provider/anthropic | 68.7% |\n"
	if got := coverMarkdown(coverRows(in), "38.2%"); got != want {
		t.Errorf("coverMarkdown =\n%s\nwant\n%s", got, want)
	}
}
