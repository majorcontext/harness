package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

type report struct {
	Requirements []requirement `json:"requirements"`
}

type requirement struct {
	ID     string `json:"id"`
	Tier   string `json:"tier"`
	Status string `json:"status"`
}

// readAllow reads lines of "ID reason". A blank line or a line that starts
// with # is ignored. A line without a reason is an error.
func readAllow(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	allowed := map[string]string{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		id, reason, _ := strings.Cut(line, " ")
		if strings.TrimSpace(reason) == "" {
			return nil, fmt.Errorf("%s:%d: %s has no reason", path, n, id)
		}
		allowed[id] = strings.TrimSpace(reason)
	}
	return allowed, sc.Err()
}

func failing(r requirement) bool { return r.Status == "FAIL" || r.Status == "NOT_TESTED" }

// judge prints the tier table and reports whether every failing MANDATORY
// requirement is on the allow-list. An allow-list entry for a requirement that passes is
// stale and fails the judgement.
func judge(w io.Writer, rep report, allowed map[string]string) bool {
	ok := true
	seen := map[string]bool{}
	for _, tier := range []string{"MANDATORY", "CAPABILITY", "ADVISORY", "INFORMATIONAL"} {
		counts := map[string]int{}
		var rows []requirement
		for _, r := range rep.Requirements {
			if r.Tier == tier {
				counts[r.Status]++
				rows = append(rows, r)
			}
		}
		fmt.Fprintf(w, "\n%s: %v\n", tier, counts)
		slices.SortFunc(rows, func(a, b requirement) int { return strings.Compare(a.ID, b.ID) })
		for _, r := range rows {
			if !failing(r) {
				continue
			}
			note := ""
			if tier == "MANDATORY" {
				if reason, listed := allowed[r.ID]; listed {
					note = "  allowed: " + reason
					seen[r.ID] = true
				} else {
					note = "  NOT ALLOWED"
					ok = false
				}
			}
			fmt.Fprintf(w, "  %-12s %s%s\n", r.Status, r.ID, note)
		}
	}
	for id := range allowed {
		if !seen[id] {
			fmt.Fprintf(w, "\nstale allow-list entry: %s passes or is not MANDATORY\n", id)
			ok = false
		}
	}
	return ok
}
