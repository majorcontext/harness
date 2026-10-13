package main

import (
	"io"
	"testing"
)

func TestJudgeFailsClosed(t *testing.T) {
	pass := requirement{ID: "A", Tier: "MANDATORY", Status: "PASS"}
	fail := requirement{ID: "A", Tier: "MANDATORY", Status: "FAIL"}
	cases := []struct {
		name    string
		rep     report
		allowed map[string]string
		want    bool
	}{
		{"empty report", report{}, nil, false},
		{"no MANDATORY tier", report{Requirements: []requirement{{ID: "C", Tier: "CAPABILITY", Status: "PASS"}}}, nil, false},
		{"all pass", report{Requirements: []requirement{pass}}, nil, true},
		{"unlisted failure", report{Requirements: []requirement{fail}}, nil, false},
		{"listed failure", report{Requirements: []requirement{fail}}, map[string]string{"A": "why"}, true},
		{"listed id absent", report{Requirements: []requirement{pass}}, map[string]string{"Z": "why"}, false},
		{"listed id passes", report{Requirements: []requirement{pass}}, map[string]string{"A": "why"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := judge(io.Discard, c.rep, c.allowed); got != c.want {
				t.Fatalf("judge = %v, want %v", got, c.want)
			}
		})
	}
}
