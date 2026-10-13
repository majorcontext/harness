//go:build js

// Package jshost adapts a JavaScript host to the seams of harness on
// GOOS=js: the fetch transport, JavaScript tools, the frame channel, and the
// readers of host options. It builds only for js.
package jshost

import (
	"context"
	"errors"
	"fmt"
	"syscall/js"
)

// JSError carries the readable text of a JavaScript value that was thrown or
// that rejected a promise.
type JSError struct{ Name, Message string }

func (e *JSError) Error() string {
	switch {
	case e.Name == "":
		return e.Message
	case e.Message == "":
		return e.Name
	}
	return e.Name + ": " + e.Message
}

// ErrorOf reads a thrown value. An Error gives its name and message. A string,
// a number, or a boolean gives its text. Any other value gives a fixed text
// that names its JavaScript type, so the text never holds an object marker.
func ErrorOf(v js.Value) *JSError {
	switch v.Type() {
	case js.TypeObject, js.TypeFunction:
		e := &JSError{}
		if msg := v.Get("message"); msg.Type() == js.TypeString {
			e.Message = msg.String()
		}
		if name := v.Get("name"); name.Type() == js.TypeString {
			e.Name = name.String()
		}
		if e.Message != "" || e.Name != "" {
			return e
		}
	case js.TypeString, js.TypeNumber, js.TypeBoolean:
		return &JSError{Message: js.Global().Call("String", v).String()}
	}
	return &JSError{Message: "non-Error rejection of type " + v.Type().String()}
}

// present says that a host option has a value.
func present(v js.Value) bool { return v.Type() != js.TypeUndefined && v.Type() != js.TypeNull }

func isArray(v js.Value) bool {
	return v.Type() == js.TypeObject && js.Global().Get("Array").Call("isArray", v).Bool()
}

// Object returns o.k when o is an object, and undefined otherwise.
func Object(o js.Value, k string) js.Value {
	if o.Type() != js.TypeObject {
		return js.Undefined()
	}
	return o.Get(k)
}

// StringField returns the string property k of o. An absent property is "". A
// property of another type is an error.
func StringField(o js.Value, k string) (string, error) {
	v := o.Get(k)
	switch {
	case !present(v):
		return "", nil
	case v.Type() != js.TypeString:
		return "", fmt.Errorf("%s must be a string", k)
	}
	return v.String(), nil
}

// StringMap copies an object whose values are strings. An absent value is an
// empty map; an array, a primitive, or a value that is not a string is an error.
func StringMap(name string, o js.Value) (map[string]string, error) {
	out := map[string]string{}
	if !present(o) {
		return out, nil
	}
	if o.Type() != js.TypeObject || isArray(o) {
		return nil, fmt.Errorf("%s must be an object of strings", name)
	}
	keys := js.Global().Get("Object").Call("keys", o)
	for i := 0; i < keys.Length(); i++ {
		k := keys.Index(i).String()
		v := o.Get(k)
		if v.Type() != js.TypeString {
			return nil, fmt.Errorf("%s.%s must be a string", name, k)
		}
		out[k] = v.String()
	}
	return out, nil
}

// StringList copies an array of strings. An absent value is nil; a value that
// is not an array, or an element that is not a string, is an error.
func StringList(name string, a js.Value) ([]string, error) {
	if !present(a) {
		return nil, nil
	}
	if !isArray(a) {
		return nil, fmt.Errorf("%s must be an array of strings", name)
	}
	out := make([]string, 0, a.Length())
	for i := 0; i < a.Length(); i++ {
		e := a.Index(i)
		if e.Type() != js.TypeString {
			return nil, fmt.Errorf("%s[%d] must be a string", name, i)
		}
		out = append(out, e.String())
	}
	return out, nil
}

// recovered turns a JavaScript exception that a call raised into err.
func recovered(err *error) {
	if r := recover(); r != nil {
		e, ok := r.(js.Error)
		if !ok {
			panic(r)
		}
		*err = ErrorOf(e.Value)
	}
}

// JSONText returns the JSON of a JavaScript value. A value that JSON.stringify
// rejects, such as a cyclic object or a BigInt, and a value with no JSON form,
// give an error.
func JSONText(v js.Value) (text string, err error) {
	defer recovered(&err)
	s := js.Global().Get("JSON").Call("stringify", v)
	if s.Type() != js.TypeString {
		return "", errors.New("value has no JSON form")
	}
	return s.String(), nil
}

// settled is the outcome of a promise: a value, or the *JSError of a rejection.
type settled struct {
	value js.Value
	err   error
}

// Await blocks the calling goroutine, never the event loop, until p settles or
// ctx ends. A rejection returns a *JSError. Each js.Func releases itself after
// it fires, and a promise that settles after ctx ended finds a buffered channel.
func Await(ctx context.Context, p js.Value) (js.Value, error) {
	ch := make(chan settled, 1)
	var ok, bad js.Func
	release := func() { ok.Release(); bad.Release() }
	ok = js.FuncOf(func(_ js.Value, a []js.Value) any {
		ch <- settled{value: a[0]}
		go release()
		return nil
	})
	bad = js.FuncOf(func(_ js.Value, a []js.Value) any {
		ch <- settled{err: ErrorOf(a[0])}
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
