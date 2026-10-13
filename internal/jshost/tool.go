//go:build js

package jshost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"syscall/js"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/protocol"
)

// jsTool is a harness.Tool that a JavaScript object implements:
// {name, description, schema, run(input, {signal})}. run returns a string, an
// object {text, isError}, or a promise of either.
type jsTool struct {
	spec protocol.ToolSpec
	obj  js.Value
}

// ReadTools reads the tools array of the harnessConnect options. An absent value
// is no tools. A value that is not an array, or an element without a name and a
// run function, is an error.
func ReadTools(list js.Value) ([]harness.Tool, error) {
	if !present(list) {
		return nil, nil
	}
	if !isArray(list) {
		return nil, errors.New("tools must be an array")
	}
	var tools []harness.Tool
	for i := 0; i < list.Length(); i++ {
		t, err := readTool(list.Index(i))
		if err != nil {
			return nil, fmt.Errorf("tools[%d]: %w", i, err)
		}
		tools = append(tools, t)
	}
	return tools, nil
}

func readTool(o js.Value) (harness.Tool, error) {
	if o.Type() != js.TypeObject {
		return nil, errors.New("must be an object")
	}
	name, err := StringField(o, "name")
	if err != nil {
		return nil, err
	}
	desc, err := StringField(o, "description")
	if err != nil {
		return nil, err
	}
	if name == "" || o.Get("run").Type() != js.TypeFunction {
		return nil, errors.New("needs a name and a run function")
	}
	schema := `{"type":"object"}`
	switch s := o.Get("schema"); s.Type() {
	case js.TypeString:
		schema = s.String()
	case js.TypeObject:
		if schema, err = JSONText(s); err != nil {
			return nil, fmt.Errorf("schema: %w", err)
		}
	case js.TypeUndefined, js.TypeNull:
	default:
		return nil, errors.New("schema must be an object or a JSON string")
	}
	if !json.Valid([]byte(schema)) {
		return nil, errors.New("schema is not JSON")
	}
	return jsTool{obj: o, spec: protocol.ToolSpec{Name: name, Description: desc, InputSchema: json.RawMessage(schema)}}, nil
}

func (t jsTool) Spec() protocol.ToolSpec { return t.spec }

// Run calls the JavaScript function on a runtime goroutine, never the event
// loop, so it may wait for the promise. An abort of ctx reaches run through
// the signal of its second argument.
func (t jsTool) Run(ctx context.Context, call harness.Call) (res protocol.ToolResult, err error) {
	defer func() {
		if err != nil {
			if je, ok := errors.AsType[*JSError](err); ok {
				err = fmt.Errorf("tool %s: %w", t.spec.Name, je)
			}
		}
	}()
	defer recovered(&err)
	args := call.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	input := js.Global().Get("JSON").Call("parse", string(args))
	ac := js.Global().Get("AbortController").New()
	opts := js.Global().Get("Object").New()
	opts.Set("signal", ac.Get("signal"))
	stop := context.AfterFunc(ctx, func() { ac.Call("abort") })
	defer stop()
	ret := t.obj.Call("run", input, opts)
	v, err := Await(ctx, js.Global().Get("Promise").Call("resolve", ret))
	if err != nil {
		return protocol.ToolResult{}, err
	}
	return resultOf(v)
}

// resultOf reads the value that run returned.
func resultOf(v js.Value) (protocol.ToolResult, error) {
	switch v.Type() {
	case js.TypeUndefined, js.TypeNull:
		return protocol.ToolResult{}, nil
	case js.TypeString:
		return protocol.ToolResult{Text: v.String()}, nil
	case js.TypeObject:
		if text := v.Get("text"); text.Type() == js.TypeString {
			return protocol.ToolResult{Text: text.String(), IsError: v.Get("isError").Truthy()}, nil
		}
	}
	text, err := JSONText(v)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	return protocol.ToolResult{Text: text}, nil
}
