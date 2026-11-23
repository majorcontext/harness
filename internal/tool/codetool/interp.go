package codetool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	starjson "go.starlark.net/lib/json"
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"

	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

var fileOptions = &syntax.FileOptions{Set: true, While: true, TopLevelControl: true, GlobalReassign: true}

const resultName = "__result__"

// script is the state of one run of one script.
type script struct {
	ctx    context.Context
	n      turn.Nested
	parent string
	seq    atomic.Int64

	mu      sync.Mutex
	out     strings.Builder
	cut     bool
	held    int
	blobs   map[int64][]protocol.Blob
	threads []*starlark.Thread
	reason  string
}

// lane orders the first nested call of the function of a parallel call after the first call of the function before it.
type lane struct {
	g       turn.Group
	ctx     context.Context
	wait    <-chan struct{}
	release func()
	first   bool
}

func run(ctx context.Context, n turn.Nested, parent, src string) protocol.ToolResult {
	s := &script{ctx: ctx, n: n, parent: parent, blobs: map[int64][]protocol.Blob{}}
	thread := s.newThread("code")
	stop := context.AfterFunc(ctx, s.cancelAll)
	defer stop()
	predeclared := s.predeclared()
	value, err := s.exec(thread, src, predeclared)
	var final string
	if err == nil && value != nil && value != starlark.None {
		final, err = s.encode(value)
	}
	text := s.output()
	if err != nil {
		msg := failure(err)
		if cause := context.Cause(ctx); cause != nil {
			msg += "\nThe script stopped: " + cause.Error()
		}
		return protocol.ToolResult{Text: join(text, msg), IsError: true}
	}
	text = join(text, final)
	res := protocol.ToolResult{Text: text}
	res.Blobs, res.Text = s.joinBlobs(res.Text)
	if res.Text == "" {
		res.Text = "(the script printed nothing and ended with no value)"
	}
	return res
}

func join(a, b string) string {
	if a == "" || b == "" {
		return a + b
	}
	return strings.TrimSuffix(a, "\n") + "\n" + b
}

func failure(err error) string {
	var ev *starlark.EvalError
	if errors.As(err, &ev) {
		return strings.TrimSpace(ev.Backtrace())
	}
	return strings.TrimSpace(err.Error())
}

func (s *script) newThread(name string) *starlark.Thread {
	th := &starlark.Thread{Name: name, Print: s.print}
	th.SetMaxExecutionSteps(MaxSteps)
	s.mu.Lock()
	s.threads = append(s.threads, th)
	reason := s.reason
	s.mu.Unlock()
	if reason != "" {
		th.Cancel(reason)
	}
	return th
}

func (s *script) cancelAll() {
	reason := "interrupted"
	if c := context.Cause(s.ctx); c != nil {
		reason = c.Error()
	}
	s.mu.Lock()
	s.reason = reason
	threads := slices.Clone(s.threads)
	s.mu.Unlock()
	for _, th := range threads {
		th.Cancel(reason)
	}
}

func (s *script) print(_ *starlark.Thread, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cut {
		return
	}
	if s.out.Len()+len(msg)+1 > MaxPrintBytes {
		s.cut = true
		return
	}
	s.out.WriteString(msg)
	s.out.WriteByte('\n')
}

func (s *script) output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.out.String()
	if s.cut {
		out += fmt.Sprintf("[printed output cut at %d bytes]\n", MaxPrintBytes)
	}
	return out
}

// exec runs src and returns the value of its last expression, or nil when it ends with another statement.
func (s *script) exec(thread *starlark.Thread, src string, predeclared starlark.StringDict) (starlark.Value, error) {
	f, err := fileOptions.Parse("script", src, 0)
	if err != nil {
		return nil, err
	}
	last := false
	if n := len(f.Stmts); n > 0 {
		if es, ok := f.Stmts[n-1].(*syntax.ExprStmt); ok {
			pos := syntax.Start(es)
			f.Stmts[n-1] = &syntax.AssignStmt{OpPos: pos, Op: syntax.EQ, LHS: &syntax.Ident{NamePos: pos, Name: resultName}, RHS: es.X}
			last = true
		}
	}
	prog, err := starlark.FileProgram(f, predeclared.Has)
	if err != nil {
		return nil, err
	}
	globals, err := prog.Init(thread, predeclared)
	if err != nil || !last {
		return nil, err
	}
	return globals[resultName], nil
}

func (s *script) encode(v starlark.Value) (string, error) {
	if err := checkBounds(v); err != nil {
		return "", err
	}
	enc := starjson.Module.Members["encode"]
	r, err := starlark.Call(s.newThread("encode"), enc, starlark.Tuple{v}, nil)
	if err != nil {
		return "", err
	}
	str, _ := starlark.AsString(r)
	return str, nil
}

func (s *script) joinBlobs(text string) ([]protocol.Blob, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []protocol.Blob
	for _, k := range slices.Sorted(maps.Keys(s.blobs)) {
		all = append(all, s.blobs[k]...)
	}
	if len(all) <= MaxBlobs {
		return all, text
	}
	note := fmt.Sprintf("[%d blobs of nested results were left out; at most %d join the result]", len(all)-MaxBlobs, MaxBlobs)
	return all[:MaxBlobs], join(text, note)
}

func (s *script) predeclared() starlark.StringDict {
	d := starlark.StringDict{}
	for _, spec := range s.n.Specs() {
		if !notCallable[spec.Name] {
			d[spec.Name] = starlark.NewBuiltin(spec.Name, s.toolFn(spec.Name))
		}
	}
	d["attempt"] = starlark.NewBuiltin("attempt", s.attempt)
	d["parallel"] = starlark.NewBuiltin("parallel", s.parallel)
	d["search_tools"] = starlark.NewBuiltin("search_tools", s.searchTools)
	d["describe_tool"] = starlark.NewBuiltin("describe_tool", s.describeTool)
	d["json"] = &starlarkstruct.Module{Name: "json", Members: starlark.StringDict{
		"encode": starlark.NewBuiltin("json.encode", s.boundedEncode), "decode": starjson.Module.Members["decode"]}}
	return d
}

func (s *script) toolFn(name string) func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
	return func(th *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if len(args) > 0 {
			return nil, fmt.Errorf("%s takes keyword arguments only", name)
		}
		res, err := s.call(th, name, kwargs)
		if err != nil {
			return nil, err
		}
		if res.IsError {
			return nil, errors.New(errText(name, res.Text))
		}
		return starlark.String(res.Text), nil
	}
}

func errText(name, text string) string {
	if text == "" {
		return name + " failed"
	}
	return text
}

// call runs one nested call for th and returns its result, or an error that stops the script.
func (s *script) call(th *starlark.Thread, name string, kwargs []starlark.Tuple) (protocol.ToolResult, error) {
	if notCallable[name] {
		return protocol.ToolResult{Text: "no such tool available: " + name, IsError: true}, nil
	}
	obj := map[string]any{}
	for _, kv := range kwargs {
		k, _ := starlark.AsString(kv[0])
		v, err := toGo(kv[1])
		if err != nil {
			return protocol.ToolResult{}, fmt.Errorf("%s: argument %s: %w", name, k, err)
		}
		obj[k] = v
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	slot := s.seq.Add(1)
	c := protocol.ToolCall{ID: fmt.Sprintf("%s.%d", s.parent, slot), Name: name, Arguments: data}
	var res protocol.ToolResult
	if l, ok := th.Local("lane").(*lane); ok {
		res, err = l.call(c)
	} else {
		g := s.n.Group(s.ctx, s.parent)
		res = g.Submit(c)()
		g.Close()
	}
	if err != nil {
		return res, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held += len(res.Text)
	if s.held > MaxHeldBytes {
		return res, fmt.Errorf("the script holds more than %d bytes of result text", MaxHeldBytes)
	}
	if len(res.Blobs) > 0 {
		s.blobs[slot] = res.Blobs
	}
	return res, nil
}

func (l *lane) call(c protocol.ToolCall) (protocol.ToolResult, error) {
	if !l.first {
		l.first = true
		if l.wait != nil {
			select {
			case <-l.wait:
			case <-l.ctx.Done():
				return protocol.ToolResult{}, context.Cause(l.ctx)
			}
		}
		wait := l.g.Submit(c)
		l.release()
		return wait(), nil
	}
	return l.g.Submit(c)(), nil
}

func (s *script) boundedEncode(th *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) == 1 && len(kwargs) == 0 {
		if err := checkBounds(args[0]); err != nil {
			return nil, fmt.Errorf("json.encode: %w", err)
		}
	}
	return starjson.Module.Members["encode"].(*starlark.Builtin).CallInternal(th, args, kwargs)
}
