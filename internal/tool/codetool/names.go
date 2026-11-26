package codetool

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"go.starlark.net/resolve"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

func toolAlias(name string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			return r
		}
		return '_'
	}, name)
}

func (s *script) bindAliases(d starlark.StringDict) {
	reserved := make(map[string]bool)
	for name := range starlark.Universe {
		reserved[name] = true
	}
	for name := range d {
		reserved[name] = true
	}
	aliases := make(map[string][]string)
	for _, spec := range s.n.Specs() {
		reserved[spec.Name] = true
		alias := toolAlias(spec.Name)
		if alias != spec.Name {
			aliases[alias] = append(aliases[alias], spec.Name)
		}
	}
	for alias, names := range aliases {
		if len(names) != 1 || reserved[alias] || notCallable[names[0]] {
			continue
		}
		expr, err := fileOptions.ParseExpr("alias", alias, 0)
		if _, ok := expr.(*syntax.Ident); err != nil || !ok {
			continue
		}
		d[alias] = starlark.NewBuiltin(names[0], s.toolFn(names[0]))
	}
}

func (s *script) nameError(err error) error {
	var errs resolve.ErrorList
	if !errors.As(err, &errs) || !strings.HasPrefix(errs[0].Msg, "undefined: ") {
		return err
	}
	name := strings.Fields(strings.TrimPrefix(errs[0].Msg, "undefined: "))[0]
	if notCallable[name] {
		return err
	}
	best := int(^uint(0) >> 1)
	var names []string
	for _, spec := range s.callable() {
		distance := editDistance(name, toolAlias(spec.Name))
		if distance > best {
			continue
		}
		if distance < best {
			best, names = distance, nil
		}
		names = append(names, spec.Name)
	}
	if len(names) == 0 {
		return err
	}
	slices.Sort(names)
	for i, name := range names[:min(len(names), 3)] {
		names[i] = fmt.Sprintf("call(%q, ...)", name)
	}
	return fmt.Errorf("%s: undefined: %s; try %s", errs[0].Pos, name, strings.Join(names[:min(len(names), 3)], " or "))
}

func editDistance(a, b string) int {
	x, y := []rune(a), []rune(b)
	row := make([]int, len(y)+1)
	for j := range row {
		row[j] = j
	}
	for i, r := range x {
		prev := row[0]
		row[0] = i + 1
		for j, s := range y {
			cost := 0
			if r != s {
				cost = 1
			}
			prev, row[j+1] = row[j+1], min(row[j]+1, row[j+1]+1, prev+cost)
		}
	}
	return row[len(y)]
}
