package gates

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var (
	tickedTokenRE  = regexp.MustCompile("`([A-Z]+ /[^`]*)`")
	packageTokenRE = regexp.MustCompile("^\\| `(harness[^`]*)` \\|")
	actionsRE      = regexp.MustCompile("`(\\w+)`, `(\\w+)`, and `(\\w+)` reply with the status")
	errorRowRE     = regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\| ([0-9]{3}) \\|$")
)

// SpecRoutes returns the "METHOD /path" of each route in the route list of
// the HTTP section of the spec, sorted. The list is the fenced block under
// "### Routes". A line holds several routes joined by "·", and the route
// "POST /processes/{name}/{action}" stands for the actions that the processes
// section names as the routes that reply with the status.
func SpecRoutes(spec string) ([]string, error) {
	block, err := fenced(spec, "### Routes")
	if err != nil {
		return nil, err
	}
	actions := actionsRE.FindStringSubmatch(spec)
	if actions == nil {
		return nil, errors.New("the processes section of the spec names no process actions")
	}
	var out []string
	for _, line := range strings.Split(block, "\n") {
		for _, part := range strings.Split(line, "·") {
			f := strings.Fields(part)
			if len(f) < 2 {
				continue
			}
			if !strings.HasPrefix(f[1], "/") {
				return nil, fmt.Errorf("route list line %q: %q is not a path", strings.TrimSpace(part), f[1])
			}
			route, _, _ := strings.Cut(f[1], "?")
			if strings.HasSuffix(route, "/{action}") {
				for _, a := range actions[1:] {
					out = append(out, f[0]+" "+strings.TrimSuffix(route, "{action}")+a)
				}
				continue
			}
			out = append(out, f[0]+" "+route)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("the route list of the spec holds no route")
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// SpecReadRoutes returns the "METHOD /path" of each route that the spec says
// harness.ReadHandler serves, sorted.
func SpecReadRoutes(spec string) ([]string, error) {
	const start, end = "`harness.ReadHandler(store, queue)` is the HTTP form", " through the code of"
	i := strings.Index(spec, start)
	if i < 0 {
		return nil, errors.New("the spec has no paragraph on harness.ReadHandler")
	}
	rest := spec[i:]
	j := strings.Index(rest, end)
	if j < 0 {
		return nil, errors.New("the ReadHandler paragraph does not end its route list with " + end)
	}
	var out []string
	for _, m := range tickedTokenRE.FindAllStringSubmatch(rest[:j], -1) {
		out = append(out, m[1])
	}
	if len(out) == 0 {
		return nil, errors.New("the ReadHandler paragraph lists no route")
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// SpecPublicPackages returns the directory, relative to the module root, of
// each package in the "Public packages" table of the spec, sorted. The
// package "harness" is the module root, "".
func SpecPublicPackages(spec string) ([]string, error) {
	_, section, found := strings.Cut(spec, "### Public packages")
	if !found {
		return nil, errors.New("the spec has no Public packages section")
	}
	var out []string
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(line, "#") {
			break
		}
		if m := packageTokenRE.FindStringSubmatch(line); m != nil {
			out = append(out, strings.TrimPrefix(strings.TrimPrefix(m[1], "harness"), "/"))
		}
	}
	if len(out) == 0 {
		return nil, errors.New("the Public packages table of the spec lists no package")
	}
	slices.Sort(out)
	return out, nil
}

func fenced(spec, heading string) (string, error) {
	_, section, found := strings.Cut(spec, heading+"\n")
	if !found {
		return "", fmt.Errorf("the spec has no %q section", heading)
	}
	_, block, found := strings.Cut(section, "```\n")
	if !found {
		return "", fmt.Errorf("%q holds no fenced block", heading)
	}
	block, _, found = strings.Cut(block, "```")
	if !found {
		return "", fmt.Errorf("the fenced block under %q does not end", heading)
	}
	return block, nil
}

// SpecErrorStatuses returns the HTTP status of each error code in the Errors
// table of the spec.
func SpecErrorStatuses(spec string) (map[string]int, error) {
	_, section, found := strings.Cut(spec, "\n### Errors\n")
	if !found {
		return nil, errors.New("the spec has no Errors section")
	}
	section, _, _ = strings.Cut(section, "\n### ")
	out := map[string]int{}
	for _, m := range errorRowRE.FindAllStringSubmatch(section, -1) {
		if _, dup := out[m[1]]; dup {
			return nil, fmt.Errorf("the Errors table of the spec holds the code %q twice", m[1])
		}
		out[m[1]], _ = strconv.Atoi(m[2])
	}
	if len(out) == 0 {
		return nil, errors.New("the Errors table of the spec holds no row")
	}
	return out, nil
}
