package codetool

import (
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
)

// maxFreezeCost is the most values that a Freeze may visit for parallel. Freeze visits each element of a tuple
// and of a function every time it reaches them, so a shared tuple tree is exponential in its depth.
const maxFreezeCost = 1 << 24

type tupleKey struct {
	first *starlark.Value
	n     int
}

// coster measures the visits that Freeze makes. A list, dict, set, or struct freezes once, so it costs one
// visit after the first. A tuple and a function freeze at each reach, so their cost is memoized by identity.
// A function that reaches itself never ends in Freeze, so it costs more than the bound.
type coster struct {
	once   map[any]bool
	memo   map[any]uint64
	onPath map[*starlark.Function]bool
}

// freezeCost returns the number of visits that freezing roots makes, or maxFreezeCost+1 when it is more.
func freezeCost(roots []starlark.Value) uint64 {
	c := &coster{once: map[any]bool{}, memo: map[any]uint64{}, onPath: map[*starlark.Function]bool{}}
	var total uint64
	for _, v := range roots {
		if total = add(total, c.cost(v)); total > maxFreezeCost {
			break
		}
	}
	return total
}

func add(a, b uint64) uint64 { return min(a+b, maxFreezeCost+1) }

func (c *coster) sum(vs func(yield func(starlark.Value) bool)) uint64 {
	total := uint64(1)
	for v := range vs {
		if total = add(total, c.cost(v)); total > maxFreezeCost {
			return total
		}
	}
	return total
}

func (c *coster) cost(v starlark.Value) uint64 {
	switch v := v.(type) {
	case starlark.Tuple:
		if len(v) == 0 {
			return 1
		}
		key := tupleKey{&v[0], len(v)}
		if n, ok := c.memo[key]; ok {
			return n
		}
		n := c.sum(func(yield func(starlark.Value) bool) {
			for _, x := range v {
				if !yield(x) {
					return
				}
			}
		})
		c.memo[key] = n
		return n
	case *starlark.List:
		if c.once[v] {
			return 1
		}
		c.once[v] = true
		return c.sum(func(yield func(starlark.Value) bool) {
			for i := range v.Len() {
				if !yield(v.Index(i)) {
					return
				}
			}
		})
	case *starlark.Dict:
		if c.once[v] {
			return 1
		}
		c.once[v] = true
		return c.sum(func(yield func(starlark.Value) bool) {
			for _, kv := range v.Items() {
				if !yield(kv[0]) || !yield(kv[1]) {
					return
				}
			}
		})
	case *starlark.Set:
		if c.once[v] {
			return 1
		}
		c.once[v] = true
		return c.sum(func(yield func(starlark.Value) bool) {
			for x := range v.Elements() {
				if !yield(x) {
					return
				}
			}
		})
	case *starlarkstruct.Struct:
		if c.once[v] {
			return 1
		}
		c.once[v] = true
		return c.sum(func(yield func(starlark.Value) bool) {
			for _, name := range v.AttrNames() {
				x, _ := v.Attr(name)
				if !yield(x) {
					return
				}
			}
		})
	case *starlark.Function:
		return c.function(v)
	}
	return 1
}

func (c *coster) function(f *starlark.Function) uint64 {
	if c.onPath[f] {
		return maxFreezeCost + 1
	}
	if n, ok := c.memo[f]; ok {
		return n
	}
	c.onPath[f] = true
	defer delete(c.onPath, f)
	n := c.sum(func(yield func(starlark.Value) bool) {
		for i := range f.NumFreeVars() {
			if _, v := f.FreeVar(i); v != nil && !yield(v) {
				return
			}
		}
		for i := range f.NumParams() {
			if d := f.ParamDefault(i); d != nil && !yield(d) {
				return
			}
		}
	})
	c.memo[f] = n
	return n
}
