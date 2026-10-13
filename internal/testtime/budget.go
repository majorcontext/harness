package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type testBudget struct {
	pkg    string
	prefix string
	limit  float64
}

type budget struct {
	leaf      float64
	pkgs      map[string]float64
	pkgBase   float64
	overrides []testBudget
}

// readBudget parses the budget file. Each line is one of:
//
//	leaf SECONDS                 limit of a test with no subtests
//	package PATH|default SECONDS limit of the wall time of a package; PATH@JOB sets it for one job
//	test PATH NAME SECONDS       limit of the tests whose name starts with NAME
func readBudget(path string) (*budget, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	b := &budget{pkgs: map[string]float64{}}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		bad := fmt.Errorf("%s: bad line %q", path, line)
		if len(f) < 2 {
			return nil, bad
		}
		secs, perr := strconv.ParseFloat(f[len(f)-1], 64)
		if perr != nil || secs <= 0 {
			return nil, bad
		}
		switch {
		case f[0] == "leaf" && len(f) == 2:
			b.leaf = secs
		case f[0] == "package" && len(f) == 3 && f[1] == "default":
			b.pkgBase = secs
		case f[0] == "package" && len(f) == 3:
			b.pkgs[f[1]] = secs
		case f[0] == "test" && len(f) == 4:
			b.overrides = append(b.overrides, testBudget{pkg: f[1], prefix: f[2], limit: secs})
		default:
			return nil, bad
		}
	}
	if b.leaf == 0 || b.pkgBase == 0 {
		return nil, fmt.Errorf("%s: leaf and package default budgets are required", path)
	}
	return b, nil
}

func (b *budget) testLimit(pkg, name string) float64 {
	for _, o := range b.overrides {
		if o.pkg == pkg && strings.HasPrefix(name, o.prefix) {
			return o.limit
		}
	}
	return b.leaf
}

func (b *budget) packageLimit(pkg, job string) float64 {
	if v, ok := b.pkgs[pkg+"@"+job]; ok {
		return v
	}
	if v, ok := b.pkgs[pkg]; ok {
		return v
	}
	return b.pkgBase
}
