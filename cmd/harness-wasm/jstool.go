//go:build js

package main

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

// readTools reads the tools array of the harnessConnect options.
func readTools(list js.Value) ([]harness.Tool, error) {
	if list.Type() != js.TypeObject {
		return nil, nil
	}
	var tools []harness.Tool
	for i := 0; i < list.Length(); i++ {
		o := list.Index(i)
		name := stringField(o, "name")
		if name == "" || o.Get("run").Type() != js.TypeFunction {
			return nil, fmt.Errorf("tools[%d] needs a name and a run function", i)
		}
		schema := `{"type":"object"}`
		switch s := o.Get("schema"); s.Type() {
		case js.TypeString:
			schema = s.String()
		case js.TypeObject:
			text, err := jsonText(s)
			if err != nil {
				return nil, fmt.Errorf("tool %s: schema: %w", name, err)
			}
			schema = text
		}
		if !json.Valid([]byte(schema)) {
			return nil, fmt.Errorf("tool %s: schema is not JSON", name)
		}
		tools = append(tools, jsTool{obj: o, spec: protocol.ToolSpec{
			Name: name, Description: stringField(o, "description"), InputSchema: json.RawMessage(schema)}})
	}
	return tools, nil
}

func (t jsTool) Spec() protocol.ToolSpec { return t.spec }

// Run calls the JavaScript function on a runtime goroutine, never the event
// loop, so it may wait for the promise. An abort of ctx reaches run through
// the signal of its second argument.
func (t jsTool) Run(ctx context.Context, call harness.Call) (res protocol.ToolResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(js.Error)
			if !ok {
				panic(r)
			}
			err = fmt.Errorf("tool %s: %w", t.spec.Name, errorOf(e.Value))
		}
	}()
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
	v, err := await(ctx, js.Global().Get("Promise").Call("resolve", ret))
	if err != nil {
		if je, ok := errors.AsType[*jsError](err); ok {
			return protocol.ToolResult{}, fmt.Errorf("tool %s: %w", t.spec.Name, je)
		}
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
	text, err := jsonText(v)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	return protocol.ToolResult{Text: text}, nil
}
