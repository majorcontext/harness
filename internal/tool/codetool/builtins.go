package codetool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"

	"github.com/majorcontext/harness/protocol"
)

func (s *script) attempt(th *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) != 1 {
		return nil, errors.New("attempt: want the tool as its one positional argument")
	}
	var name string
	switch v := args[0].(type) {
	case *starlark.Builtin:
		name = v.Name()
	case starlark.String:
		name = string(v)
	default:
		return nil, fmt.Errorf("attempt: want a tool function or a tool name, got %s", v.Type())
	}
	res, err := s.call(th, name, kwargs)
	if err != nil {
		return nil, err
	}
	return starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"ok": starlark.Bool(!res.IsError), "text": starlark.String(res.Text)}), nil
}

func (s *script) parallel(th *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	fns, err := functionsOf(b, args, kwargs)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	g := s.n.Group(ctx, s.parent)
	defer g.Close()
	results := make([]starlark.Value, len(fns))
	gates := make([]chan struct{}, len(fns))
	for i := range gates {
		gates[i] = make(chan struct{})
	}
	var (
		wg       sync.WaitGroup
		once     sync.Once
		first    error
		children []*starlark.Thread
		mu       sync.Mutex
	)
	fail := func(err error) {
		once.Do(func() {
			first = err
			cancel()
			mu.Lock()
			defer mu.Unlock()
			for _, c := range children {
				c.Cancel("another function of parallel failed")
			}
		})
	}
	sem := make(chan struct{}, MaxParallel)
start:
	for i, f := range fns {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break start
		}
		child := s.newThread(fmt.Sprintf("parallel %d", i))
		l := &lane{g: g, ctx: ctx}
		l.release = sync.OnceFunc(func() { close(gates[i]) })
		if i > 0 {
			l.wait = gates[i-1]
		}
		child.SetLocal("lane", l)
		mu.Lock()
		children = append(children, child)
		mu.Unlock()
		wg.Go(func() {
			defer func() { <-sem }()
			defer l.release()
			v, err := starlark.Call(child, f, nil, nil)
			if err != nil {
				fail(err)
				return
			}
			results[i] = v
		})
	}
	wg.Wait()
	if first != nil {
		return nil, first
	}
	if err := context.Cause(s.ctx); err != nil {
		return nil, err
	}
	return starlark.NewList(results), nil
}

func functionsOf(b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) ([]starlark.Callable, error) {
	var list starlark.Value
	if err := starlark.UnpackPositionalArgs(b.Name(), args, kwargs, 1, &list); err != nil {
		return nil, err
	}
	seq, ok := list.(starlark.Indexable)
	if !ok {
		return nil, fmt.Errorf("parallel: want a list of functions, got %s", list.Type())
	}
	fns := make([]starlark.Callable, seq.Len())
	for i := range fns {
		f, ok := seq.Index(i).(starlark.Callable)
		if !ok {
			return nil, fmt.Errorf("parallel: item %d is not a function", i)
		}
		fns[i] = f
	}
	return fns, nil
}

func (s *script) callable() []protocol.ToolSpec {
	return slices.DeleteFunc(s.n.Specs(), func(t protocol.ToolSpec) bool { return notCallable[t.Name] })
}

func (s *script) searchTools(th *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var query string
	if err := starlark.UnpackPositionalArgs(b.Name(), args, kwargs, 1, &query); err != nil {
		return nil, err
	}
	tokens := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	if len(tokens) == 0 {
		return nil, errors.New("search_tools: query is empty")
	}
	type hit struct {
		spec  protocol.ToolSpec
		score int
	}
	var hits []hit
	for _, t := range s.callable() {
		name, desc := strings.ToLower(t.Name), strings.ToLower(t.Description)
		score := 0
		if strings.TrimSpace(strings.ToLower(query)) == name {
			score += 100
		}
		for _, tok := range tokens {
			if strings.Contains(name, tok) {
				score += 50
			}
			if strings.Contains(desc, tok) {
				score += 10
			}
		}
		if score > 0 {
			hits = append(hits, hit{t, score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].spec.Name < hits[j].spec.Name
	})
	var out []starlark.Value
	for _, h := range hits[:min(len(hits), 20)] {
		d := starlark.NewDict(2)
		_ = d.SetKey(starlark.String("name"), starlark.String(h.spec.Name))
		_ = d.SetKey(starlark.String("description"), starlark.String(h.spec.Description))
		out = append(out, d)
	}
	return starlark.NewList(out), nil
}

func (s *script) describeTool(th *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name string
	if err := starlark.UnpackPositionalArgs(b.Name(), args, kwargs, 1, &name); err != nil {
		return nil, err
	}
	var spec protocol.ToolSpec
	ok := !notCallable[name]
	if ok {
		spec, ok = s.n.Spec(s.ctx, name)
	}
	if !ok {
		return nil, errors.New("no such tool available: " + name)
	}
	var schema any
	if len(spec.InputSchema) > 0 {
		_ = json.Unmarshal(spec.InputSchema, &schema)
	}
	d := starlark.NewDict(3)
	_ = d.SetKey(starlark.String("name"), starlark.String(spec.Name))
	_ = d.SetKey(starlark.String("description"), starlark.String(spec.Description))
	_ = d.SetKey(starlark.String("input_schema"), fromGo(schema))
	return d, nil
}

func toGo(v starlark.Value) (any, error) {
	switch v := v.(type) {
	case starlark.NoneType:
		return nil, nil
	case starlark.Bool:
		return bool(v), nil
	case starlark.Int:
		if i, ok := v.Int64(); ok {
			return i, nil
		}
		f, _ := starlark.AsFloat(v)
		return f, nil
	case starlark.Float:
		return float64(v), nil
	case starlark.String:
		return string(v), nil
	case *starlark.List:
		return seqToGo(v)
	case starlark.Tuple:
		return seqToGo(v)
	case *starlark.Dict:
		m := map[string]any{}
		for _, kv := range v.Items() {
			k, ok := kv[0].(starlark.String)
			if !ok {
				return nil, fmt.Errorf("a dict key is %s, want string", kv[0].Type())
			}
			x, err := toGo(kv[1])
			if err != nil {
				return nil, err
			}
			m[string(k)] = x
		}
		return m, nil
	}
	return nil, fmt.Errorf("cannot pass a %s as JSON", v.Type())
}

func seqToGo(v starlark.Indexable) (any, error) {
	out := make([]any, v.Len())
	for i := range out {
		x, err := toGo(v.Index(i))
		if err != nil {
			return nil, err
		}
		out[i] = x
	}
	return out, nil
}

func fromGo(v any) starlark.Value {
	switch v := v.(type) {
	case nil:
		return starlark.None
	case bool:
		return starlark.Bool(v)
	case float64:
		if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
			return starlark.MakeInt64(int64(v))
		}
		return starlark.Float(v)
	case string:
		return starlark.String(v)
	case []any:
		out := make([]starlark.Value, len(v))
		for i, x := range v {
			out[i] = fromGo(x)
		}
		return starlark.NewList(out)
	case map[string]any:
		d := starlark.NewDict(len(v))
		for _, k := range slices.Sorted(maps.Keys(v)) {
			_ = d.SetKey(starlark.String(k), fromGo(v[k]))
		}
		return d
	}
	return starlark.None
}
