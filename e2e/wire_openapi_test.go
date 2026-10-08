package e2e

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/harness/internal/gates"
)

const (
	schemaRef     = "#/components/schemas/"
	errorBodyName = "ErrorBody"
	eventName     = "Event"
)

type specResponse struct {
	Content map[string]struct {
		Schema any `json:"schema"`
	} `json:"content"`
}

type specOperation struct {
	OperationID string                  `json:"operationId"`
	Responses   map[string]specResponse `json:"responses"`
}

type specDoc struct {
	Paths      map[string]map[string]specOperation `json:"paths"`
	Components struct {
		Schemas map[string]any `json:"schemas"`
	} `json:"components"`
}

var servedSpec = sync.OnceValues(func() (*specDoc, error) {
	data, err := os.ReadFile("../protocol/openapi.json")
	if err != nil {
		return nil, err
	}
	var doc specDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
})

func (d *specDoc) operation(method, path string) (specOperation, bool) {
	segs := strings.Split(path, "/")
	for tmpl, ops := range d.Paths {
		parts := strings.Split(tmpl, "/")
		if len(parts) != len(segs) {
			continue
		}
		match := true
		for i, p := range parts {
			if strings.HasPrefix(p, "{") && segs[i] != "" {
				continue
			}
			match = match && p == segs[i]
		}
		if op, ok := ops[strings.ToLower(method)]; match && ok {
			return op, true
		}
	}
	return specOperation{}, false
}

var specErrorStatuses = sync.OnceValues(func() (map[string]int, error) {
	data, err := os.ReadFile("../docs/architecture.md")
	if err != nil {
		return nil, err
	}
	return gates.SpecErrorStatuses(string(data))
})

func checkErrorStatus(status int, routed bool, body []byte) []string {
	table, err := specErrorStatuses()
	if err != nil {
		return []string{err.Error()}
	}
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return nil
	}
	want, ok := table[e.Error.Code]
	switch {
	case !ok:
		return []string{fmt.Sprintf("error code %q is not in the Errors table of the spec", e.Error.Code)}
	case !routed && e.Error.Code == "invalid_request" && (status == http.StatusNotFound || status == http.StatusMethodNotAllowed):
		return nil
	case want != status:
		return []string{fmt.Sprintf("error code %q answers %d, and the Errors table of the spec says %d", e.Error.Code, status, want)}
	}
	return nil
}

func (d *specDoc) schema(name string) any { return d.Components.Schemas[name] }

func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after the JSON value")
	}
	return v, nil
}

func kindOf(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case json.Number:
		if _, err := strconv.ParseInt(x.String(), 10, 64); err == nil {
			return "integer"
		}
		return "number"
	}
	return fmt.Sprintf("%T", v)
}

func typeMatches(want string, v any) bool {
	got := kindOf(v)
	return got == want || want == "number" && got == "integer"
}

func (d *specDoc) validate(schema, v any, at string) []string {
	switch s := schema.(type) {
	case bool:
		if !s {
			return []string{at + ": no value is allowed"}
		}
		return nil
	case map[string]any:
		return d.validateObject(s, v, at)
	}
	return nil
}

var supportedKeywords = []string{"$ref", "anyOf", "type", "properties", "required", "items", "additionalProperties", "format", "contentEncoding", "description", "title", "$schema", "examples", "default", "enum"}

func (d *specDoc) validateObject(s map[string]any, v any, at string) []string {
	for k := range s {
		if !slices.Contains(supportedKeywords, k) {
			return []string{at + ": the OpenAPI document uses the schema keyword " + k + ", which the wire gate does not check"}
		}
	}
	if ref, ok := s["$ref"].(string); ok {
		name, found := strings.CutPrefix(ref, schemaRef)
		if !found || d.schema(name) == nil {
			return []string{at + ": unresolved reference " + ref}
		}
		return d.validate(d.schema(name), v, at)
	}
	if alts, ok := s["anyOf"].([]any); ok {
		var all []string
		for _, alt := range alts {
			errs := d.validate(alt, v, at)
			if len(errs) == 0 {
				return nil
			}
			all = append(all, errs...)
		}
		return []string{at + ": matches no alternative: " + strings.Join(all, "; ")}
	}
	if want, ok := s["type"].(string); ok && !typeMatches(want, v) {
		return []string{fmt.Sprintf("%s: want %s, got %s", at, want, kindOf(v))}
	}
	var errs []string
	switch x := v.(type) {
	case string:
		errs = append(errs, validateString(s, x, at)...)
		errs = append(errs, validateEnum(s, x, at)...)
	case map[string]any:
		errs = append(errs, d.validateFields(s, x, at)...)
	case []any:
		if items, ok := s["items"]; ok {
			for i, e := range x {
				errs = append(errs, d.validate(items, e, fmt.Sprintf("%s[%d]", at, i))...)
			}
		}
	}
	return errs
}

func validateEnum(s map[string]any, x, at string) []string {
	enum, ok := s["enum"].([]any)
	if !ok || slices.Contains(enum, any(x)) {
		return nil
	}
	return []string{fmt.Sprintf("%s: %q is not one of %v", at, x, enum)}
}

func validateString(s map[string]any, x, at string) []string {
	if s["format"] == "date-time" {
		if _, err := time.Parse(time.RFC3339Nano, x); err != nil {
			return []string{at + ": not an RFC 3339 time: " + x}
		}
	}
	if s["contentEncoding"] == "base64" {
		if _, err := base64.StdEncoding.DecodeString(x); err != nil {
			return []string{at + ": not base64"}
		}
	}
	return nil
}

func (d *specDoc) validateFields(s map[string]any, x map[string]any, at string) []string {
	var errs []string
	if req, ok := s["required"].([]any); ok {
		for _, name := range req {
			if _, present := x[name.(string)]; !present {
				errs = append(errs, at+": missing required field "+name.(string))
			}
		}
	}
	props, _ := s["properties"].(map[string]any)
	keys := make([]string, 0, len(x))
	for k := range x {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if sub, ok := props[k]; ok {
			errs = append(errs, d.validate(sub, x[k], at+"."+k)...)
			continue
		}
		switch extra := s["additionalProperties"].(type) {
		case bool:
			if !extra {
				errs = append(errs, at+": unexpected field "+k)
			}
		case map[string]any:
			errs = append(errs, d.validate(extra, x[k], at+"."+k)...)
		case nil:
			if len(props) > 0 {
				errs = append(errs, at+": field "+k+" is not in the OpenAPI document")
			}
		}
	}
	return errs
}

func mediaType(header string) string {
	mt, _, _ := strings.Cut(header, ";")
	return strings.ToLower(strings.TrimSpace(mt))
}

func successStatus(status int) bool { return status >= 200 && status < 300 }

func (d *specDoc) checkStream(method, path string, status int, contentType string) []string {
	return d.checkResponse(method, path, status, contentType, nil)
}

func (d *specDoc) checkResponse(method, path string, status int, contentType string, body []byte) []string {
	op, known := d.operation(method, path)
	if status >= 400 || !known && !successStatus(status) {
		errs := d.checkJSON(errorBodyName, contentType, body)
		if len(errs) == 0 {
			errs = checkErrorStatus(status, known, body)
		}
		return errs
	}
	if !known {
		return []string{"route is not in the OpenAPI document"}
	}
	res, ok := op.Responses[strconv.Itoa(status)]
	if !ok {
		return []string{fmt.Sprintf("%s answers status %d, which the OpenAPI document does not list", op.OperationID, status)}
	}
	if len(res.Content) == 0 {
		if len(body) > 0 {
			return []string{fmt.Sprintf("%s answers %d with a body, and the document lists none", op.OperationID, status)}
		}
		return nil
	}
	media := mediaType(contentType)
	content, ok := res.Content[media]
	if !ok {
		if _, raw := res.Content["*/*"]; raw {
			return nil
		}
		return []string{fmt.Sprintf("%s answers %d as %q, and the document lists %s", op.OperationID, status, media, mediaKeys(res))}
	}
	if media == "text/event-stream" {
		return nil
	}
	return d.checkValue(content.Schema, body)
}

func mediaKeys(res specResponse) string {
	var keys []string
	for k := range res.Content {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return strings.Join(keys, ", ")
}

func (d *specDoc) checkJSON(schema, contentType string, body []byte) []string {
	if mt := mediaType(contentType); mt != "application/json" {
		return []string{fmt.Sprintf("error body has content type %q, want application/json", mt)}
	}
	return d.checkValue(map[string]any{"$ref": schemaRef + schema}, body)
}

func (d *specDoc) checkValue(schema any, body []byte) []string {
	v, err := decodeJSON(body)
	if err != nil {
		return []string{"body is not one JSON value: " + err.Error()}
	}
	return d.validate(schema, v, "$")
}

func (d *specDoc) checkFrame(event, data string) []string {
	name := eventName
	if event == "error" {
		name = errorBodyName
	}
	return d.checkValue(map[string]any{"$ref": schemaRef + name}, []byte(data))
}

type wireReport struct {
	mu   sync.Mutex
	msgs []string
	done bool
}

func (r *wireReport) add(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.done {
		r.msgs = append(r.msgs, msg)
	}
}

var wireReports sync.Map

func reportOf(t *testing.T) *wireReport {
	t.Helper()
	fresh := &wireReport{}
	if got, loaded := wireReports.LoadOrStore(t, fresh); loaded {
		return got.(*wireReport)
	}
	t.Cleanup(func() {
		wireReports.Delete(t)
		fresh.mu.Lock()
		fresh.done = true
		msgs := slices.Clone(fresh.msgs)
		fresh.mu.Unlock()
		for _, m := range slices.Compact(msgs) {
			t.Errorf("response breaks the served contract (protocol/openapi.json, spec Errors): %s", m)
		}
	})
	return fresh
}

type wireTransport struct {
	base http.RoundTripper
	rep  *wireReport
}

func (w wireTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := w.base.RoundTrip(req)
	if err != nil || req.Method == http.MethodOptions {
		return resp, err
	}
	doc, err := servedSpec()
	if err != nil {
		w.rep.add("OpenAPI document: " + err.Error())
		return resp, nil
	}
	label := fmt.Sprintf("%s %s -> %d", req.Method, req.URL.Path, resp.StatusCode)
	contentType := resp.Header.Get("Content-Type")
	if mediaType(contentType) == "text/event-stream" && successStatus(resp.StatusCode) {
		for _, m := range doc.checkStream(req.Method, req.URL.Path, resp.StatusCode, contentType) {
			w.rep.add(label + ": " + m)
		}
		resp.Body = &frameBody{rc: resp.Body, check: func(event, data string) {
			for _, m := range doc.checkFrame(event, data) {
				w.rep.add(label + " frame: " + m)
			}
		}}
		return resp, nil
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if err == nil {
		for _, m := range doc.checkResponse(req.Method, req.URL.Path, resp.StatusCode, contentType, body) {
			w.rep.add(label + ": " + m)
		}
	}
	return resp, err
}

type frameBody struct {
	rc    io.ReadCloser
	buf   []byte
	check func(event, data string)
}

func (b *frameBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.buf = append(b.buf, p[:n]...)
	for {
		i := bytes.Index(b.buf, []byte("\n\n"))
		if i < 0 {
			break
		}
		b.frame(string(b.buf[:i]))
		b.buf = b.buf[i+2:]
	}
	if err == io.EOF && len(bytes.TrimSpace(b.buf)) > 0 {
		b.frame(string(b.buf))
		b.buf = nil
	}
	return n, err
}

func (b *frameBody) frame(text string) {
	var event string
	var data []string
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = append(data, strings.TrimPrefix(line, "data: "))
		}
	}
	if len(data) > 0 {
		b.check(event, strings.Join(data, "\n"))
	}
}

func (b *frameBody) Close() error { return b.rc.Close() }

func wireClient(t *testing.T, c *http.Client) *http.Client {
	t.Helper()
	return wireClientFor(reportOf(t), c)
}

func wireClientFor(rep *wireReport, c *http.Client) *http.Client {
	base := c.Transport
	if _, wrapped := base.(wireTransport); wrapped || rep == nil {
		return c
	}
	if base == nil {
		base = http.DefaultTransport
	}
	clone := *c
	clone.Transport = wireTransport{base: base, rep: rep}
	return &clone
}
