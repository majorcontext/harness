//go:build js

package main

import (
	"context"
	"errors"
	"syscall/js"
)

// jsError carries the readable text of a JavaScript value that was thrown or
// that rejected a promise.
type jsError struct{ name, message string }

func (e *jsError) Error() string {
	switch {
	case e.name == "":
		return e.message
	case e.message == "":
		return e.name
	}
	return e.name + ": " + e.message
}

// errorOf reads a thrown value. An Error gives its name and message, any other
// value its string form, so the text never holds an object marker.
func errorOf(v js.Value) *jsError {
	if v.Type() == js.TypeObject {
		if msg := v.Get("message"); msg.Type() == js.TypeString {
			e := &jsError{message: msg.String()}
			if name := v.Get("name"); name.Type() == js.TypeString {
				e.name = name.String()
			}
			return e
		}
	}
	return &jsError{message: js.Global().Call("String", v).String()}
}

// stringField returns the string property k of o, or "" when it is not a string.
func stringField(o js.Value, k string) string {
	if v := o.Get(k); v.Type() == js.TypeString {
		return v.String()
	}
	return ""
}

// stringMap copies the string properties of a JavaScript object.
func stringMap(o js.Value) map[string]string {
	out := map[string]string{}
	if o.Type() != js.TypeObject {
		return out
	}
	keys := js.Global().Get("Object").Call("keys", o)
	for i := 0; i < keys.Length(); i++ {
		k := keys.Index(i).String()
		if v := o.Get(k); v.Type() == js.TypeString {
			out[k] = v.String()
		}
	}
	return out
}

// stringList copies a JavaScript array of strings. It returns nil for a value
// that is not an array.
func stringList(a js.Value) []string {
	if a.Type() != js.TypeObject || !js.Global().Get("Array").Call("isArray", a).Bool() {
		return nil
	}
	out := make([]string, 0, a.Length())
	for i := 0; i < a.Length(); i++ {
		out = append(out, js.Global().Call("String", a.Index(i)).String())
	}
	return out
}

// jsonText returns the JSON of a JavaScript value.
func jsonText(v js.Value) (string, error) {
	s := js.Global().Get("JSON").Call("stringify", v)
	if s.Type() != js.TypeString {
		return "", errors.New("value has no JSON form")
	}
	return s.String(), nil
}

// settled is the outcome of a promise: a value, or the *jsError of a rejection.
type settled struct {
	value js.Value
	err   error
}

// await blocks the calling goroutine, never the event loop, until p settles or
// ctx ends. A rejection returns a *jsError. Each js.Func releases itself after
// it fires, and a promise that settles after ctx ended finds a buffered channel.
func await(ctx context.Context, p js.Value) (js.Value, error) {
	ch := make(chan settled, 1)
	var ok, bad js.Func
	release := func() { ok.Release(); bad.Release() }
	ok = js.FuncOf(func(_ js.Value, a []js.Value) any {
		ch <- settled{value: a[0]}
		go release()
		return nil
	})
	bad = js.FuncOf(func(_ js.Value, a []js.Value) any {
		ch <- settled{err: errorOf(a[0])}
		go release()
		return nil
	})
	p.Call("then", ok, bad)
	select {
	case s := <-ch:
		return s.value, s.err
	case <-ctx.Done():
		return js.Undefined(), ctx.Err()
	}
}
