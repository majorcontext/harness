// Command testtime reads the output of go test -json on standard input, prints
// the readable test log, and fails when a test or a package is slower than the
// budget of testdata/test-time-budget.txt. Run it from the repository root.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

const modulePath = "github.com/majorcontext/harness"

type event struct {
	Action  string
	Package string
	Test    string
	Output  string
	Elapsed float64
}

type result struct {
	pkg     string
	name    string
	elapsed float64
}

type report struct {
	tests    []result
	packages []result
	parents  map[string]bool
	failed   bool
}

func main() {
	budgetFile := flag.String("budget", "testdata/test-time-budget.txt", "budget file")
	summaryFile := flag.String("summary", os.Getenv("GITHUB_STEP_SUMMARY"), "file that receives a Markdown summary")
	title := flag.String("title", "", "heading of the step summary")
	flag.Parse()
	budget, err := readBudget(*budgetFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testtime:", err)
		os.Exit(1)
	}
	rep := consume(os.Stdin, os.Stdout)
	over := rep.check(budget)
	if path := *summaryFile; path != "" {
		if err := appendSummary(path, *title, rep, over); err != nil {
			fmt.Fprintln(os.Stderr, "testtime:", err)
		}
	}
	for _, line := range over {
		fmt.Fprintln(os.Stderr, "testtime: over budget:", line)
	}
	if len(over) > 0 || rep.failed {
		os.Exit(1)
	}
}

// consume prints the log of the stream: package lines at once and the output
// of a test only when that test fails.
func consume(in io.Reader, out io.Writer) *report {
	rep := &report{parents: map[string]bool{}}
	held := map[string][]string{}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Text()
		var ev event
		if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Action == "" {
			_, _ = fmt.Fprintln(out, line)
			continue
		}
		key := ev.Package + "\x00" + ev.Test
		switch ev.Action {
		case "output":
			if ev.Test == "" {
				_, _ = fmt.Fprint(out, ev.Output)
			} else {
				held[key] = append(held[key], ev.Output)
			}
		case "pass", "fail":
			rep.record(ev)
			if ev.Action == "fail" {
				rep.failed = true
				for _, text := range held[key] {
					_, _ = fmt.Fprint(out, text)
				}
			}
			delete(held, key)
		case "skip":
			delete(held, key)
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "testtime:", err)
		rep.failed = true
	}
	return rep
}

func (r *report) record(ev event) {
	pkg := strings.TrimPrefix(strings.TrimPrefix(ev.Package, modulePath), "/")
	if pkg == "" {
		pkg = "."
	}
	if ev.Test == "" {
		r.packages = append(r.packages, result{pkg: pkg, elapsed: ev.Elapsed})
		return
	}
	r.tests = append(r.tests, result{pkg: pkg, name: ev.Test, elapsed: ev.Elapsed})
	if i := strings.LastIndex(ev.Test, "/"); i >= 0 {
		r.parents[pkg+"\x00"+ev.Test[:i]] = true
	}
}

// check compares every leaf test and every package with its budget. A test
// with subtests is not a leaf: its time is the sum of the time of its parts.
func (r *report) check(b *budget) []string {
	var over []string
	for _, t := range r.tests {
		if r.parents[t.pkg+"\x00"+t.name] {
			continue
		}
		if limit := b.testLimit(t.pkg, t.name); t.elapsed > limit {
			over = append(over, fmt.Sprintf("%s %s took %.1fs, budget %.1fs", t.pkg, t.name, t.elapsed, limit))
		}
	}
	for _, p := range r.packages {
		if limit := b.packageLimit(p.pkg); p.elapsed > limit {
			over = append(over, fmt.Sprintf("package %s took %.1fs, budget %.1fs", p.pkg, p.elapsed, limit))
		}
	}
	return over
}

func appendSummary(path, title string, r *report, over []string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if title == "" {
		title = "Test time"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "### %s: slowest tests\n\n| Seconds | Package | Test |\n| ---: | --- | --- |\n", title)
	leaves := make([]result, 0, len(r.tests))
	for _, t := range r.tests {
		if !r.parents[t.pkg+"\x00"+t.name] {
			leaves = append(leaves, t)
		}
	}
	sort.Slice(leaves, func(i, j int) bool { return leaves[i].elapsed > leaves[j].elapsed })
	for _, t := range leaves[:min(10, len(leaves))] {
		fmt.Fprintf(&b, "| %.2f | `%s` | `%s` |\n", t.elapsed, t.pkg, t.name)
	}
	fmt.Fprint(&b, "\nSlowest packages\n\n| Seconds | Package |\n| ---: | --- |\n")
	pkgs := append([]result(nil), r.packages...)
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].elapsed > pkgs[j].elapsed })
	for _, p := range pkgs[:min(5, len(pkgs))] {
		fmt.Fprintf(&b, "| %.2f | `%s` |\n", p.elapsed, p.pkg)
	}
	for _, line := range over {
		fmt.Fprintf(&b, "\nOver budget: %s\n", line)
	}
	b.WriteString("\n")
	_, err = f.WriteString(b.String())
	return err
}
