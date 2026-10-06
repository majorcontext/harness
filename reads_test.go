package harness_test

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/protocol"
)

func TestAGetOfASessionNeitherOwnsItNorStartsItsTurn(t *testing.T) {
	for _, path := range []string{"/sessions/s1", "/sessions/s1/events"} {
		t.Run(path, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				st := harness.NewMemStore()
				f1 := newFake()
				r1 := runtime(t, st, f1)
				s := create(t, r1)
				submit(t, s, text("a", "hi"))
				<-f1.runs
				closeRuntime(t, r1)
				head, err := st.Head(bg, "s1")
				if err != nil {
					t.Fatal(err)
				}
				f2 := newFake()
				r2 := runtime(t, st, f2)
				rec := httptest.NewRecorder()
				r2.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
				synctest.Wait()
				if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"s1"`) && path == "/sessions/s1" {
					t.Errorf("GET %s = %d %s, want 200 with the session", path, rec.Code, rec.Body)
				}
				if got, _ := st.Head(bg, "s1"); got != head {
					t.Errorf("head after the GET = %d, want %d: the GET appended to the log", got, head)
				}
				select {
				case <-f2.runs:
					t.Error("the GET resumed the turn of the session")
				default:
				}
				closeRuntime(t, r2)
			})
		})
	}
}

func TestAGetOfASessionThatTheRuntimeDoesNotRunListsThePlugins(t *testing.T) {
	st := harness.NewMemStore()
	r1 := runtime(t, st, newFake())
	create(t, r1)
	closeRuntime(t, r1)
	r2, err := harness.NewWithBackend(harness.Options{Store: st, Config: config.Config{Plugins: pluginFixture(t, `{}`)}}, newFake())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeRuntime(t, r2) })
	if _, err := r2.Create(bg, protocol.CreateSession{ID: "s2", Model: "fake/model"}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	r2.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s1", nil))
	var got protocol.Session
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Plugins) != 1 || got.Plugins[0].Name != "fixture" {
		t.Errorf("GET of a cold session = %d %s (%v), want the fixture plugin", rec.Code, rec.Body, err)
	}
}

const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

func pngBytes(t *testing.T) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(onePixelPNG)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// withoutPlugins returns the body with the plugins of each session cleared. When
// empty, it fails the test for a session that lists a plugin.
func withoutPlugins(t *testing.T, path string, rec *httptest.ResponseRecorder, empty bool) string {
	t.Helper()
	var v any
	switch {
	case rec.Code != http.StatusOK:
		return rec.Body.String()
	case path == "/sessions" || strings.HasPrefix(path, "/sessions?"):
		v = &protocol.SessionPage{}
	case !strings.Contains(path[1:], "/") || strings.Count(strings.SplitN(path, "?", 2)[0], "/") == 2:
		v = &protocol.Session{}
	default:
		return rec.Body.String()
	}
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	strip := func(sess *protocol.Session) {
		if empty && len(sess.Plugins) != 0 {
			t.Errorf("GET %s lists plugins %v, want none", path, sess.Plugins)
		}
		sess.Plugins = nil
	}
	switch v := v.(type) {
	case *protocol.SessionPage:
		for i := range v.Sessions {
			strip(&v.Sessions[i])
		}
	case *protocol.Session:
		strip(v)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

type frame struct {
	id   string
	data string
}

func frames(t *testing.T, h http.Handler, path string, upTo uint64) []frame {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out []frame
	var cur frame
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			cur.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		case line == "" && cur.id != "":
			out = append(out, cur)
			if n, _ := strconv.ParseUint(cur.id, 10, 64); upTo > 0 && n >= upTo {
				return out
			}
			cur = frame{}
		case line == "":
			cur = frame{}
		}
	}
	return out
}

func storedLog(t *testing.T, st harness.Store) (head uint64) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		r := runtime(t, st, f)
		s := create(t, r)
		if _, err := r.Create(bg, protocol.CreateSession{ID: "s2", Model: "fake/model"}); err != nil {
			t.Fatal(err)
		}
		in := text("a", "hi")
		in.Parts = append(in.Parts, protocol.Part{Type: protocol.PartBlob, MediaType: "image/png", Data: pngBytes(t)})
		submit(t, s, in)
		run := <-f.runs
		run.emit(say("hello"))
		run.end()
		submit(t, s, text("b", "again"))
		run = <-f.runs
		run.emit(say("again, then"))
		run.end()
		head, _ = st.Head(bg, "s1")
		closeRuntime(t, r)
	})
	return head
}

func TestTheReadHandlerAnswersAsRuntimeHandlerAnswersForTheSameLog(t *testing.T) {
	st := harness.NewMemStore()
	head := storedLog(t, st)
	r2, err := harness.NewWithBackend(harness.Options{Store: st, Config: config.Config{Plugins: pluginFixture(t, `{}`)}}, newFake())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeRuntime(t, r2) })
	ro := harness.ReadHandler(st, nil)
	for _, path := range []string{
		"/sessions", "/sessions?limit=1", "/sessions?limit=1&after=s1", "/sessions?after=nope", "/sessions/s1", "/sessions/s2", "/sessions/nope",
		"/sessions/s1/messages", "/sessions/s1/messages?limit=1", "/sessions/s1/messages?limit=1&before=3", "/sessions/s1/messages?limit=2000",
		"/sessions/nope/messages", "/sessions/s1/events", "/sessions/s1/events?after=2&limit=3", "/sessions/s1/events?after=x", "/sessions/s1/inputs",
	} {
		t.Run(path, func(t *testing.T) {
			want, got := get(t, r2.Handler(), path), get(t, ro, path)
			if got.Code != want.Code || got.Header().Get("Content-Type") != want.Header().Get("Content-Type") {
				t.Fatalf("read handler = %d %s, runtime handler = %d %s", got.Code, got.Header().Get("Content-Type"), want.Code, want.Header().Get("Content-Type"))
			}
			if w, g := withoutPlugins(t, path, want, false), withoutPlugins(t, path, got, true); w != g {
				t.Errorf("GET %s\nread handler:    %s\nruntime handler: %s", path, g, w)
			}
		})
	}
	t.Run("sse_sends_the_durable_frames_to_the_head_then_ends", func(t *testing.T) {
		got := frames(t, ro, "/sessions/s1/events?after=1", 0)
		want := frames(t, r2.Handler(), "/sessions/s1/events?after=1", head)
		if len(want) == 0 || !slices.Equal(got, want) {
			t.Fatalf("read handler frames = %v\nruntime handler frames = %v", got, want)
		}
		if last := got[len(got)-1].id; last != strconv.FormatUint(head, 10) {
			t.Errorf("last frame id = %s, want the head %d", last, head)
		}
		now, _ := st.Head(bg, "s1")
		if all := frames(t, ro, "/sessions/s1/events", 0); uint64(len(all)) != now {
			t.Errorf("a stream from the start sent %d frames, want %d: one for each record", len(all), now)
		}
		if after, _ := st.Head(bg, "s1"); after != now {
			t.Errorf("head after the read-only streams = %d, want %d", after, now)
		}
	})
}

func TestTheReadHandlerAnswersAWriteWith405AndAppendsNothing(t *testing.T) {
	st := harness.NewMemStore()
	head := storedLog(t, st)
	ro := harness.ReadHandler(st, nil)
	for _, c := range []struct{ method, path string }{
		{"POST", "/sessions"}, {"PATCH", "/sessions/s1"}, {"DELETE", "/sessions/s1"}, {"POST", "/sessions/s1/inputs"},
		{"DELETE", "/sessions/s1/inputs/a"}, {"POST", "/sessions/s1/interrupt"}, {"PUT", "/sessions/s1/goal"}, {"POST", "/sessions/s1/blobs/k"},
	} {
		t.Run(c.method+c.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			ro.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, strings.NewReader(`{}`)))
			var body protocol.ErrorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); rec.Code != http.StatusMethodNotAllowed || err != nil || body.Error.Code != protocol.CodeInvalidRequest {
				t.Errorf("%s %s = %d %s, want 405 in the error envelope", c.method, c.path, rec.Code, rec.Body)
			}
			if _, ok := rec.Header()["Allow"]; !ok {
				t.Errorf("%s %s answers 405 with no Allow header", c.method, c.path)
			}
		})
	}
	if got, _ := st.Head(bg, "s1"); got != head {
		t.Errorf("head after the writes = %d, want %d", got, head)
	}
	if ids, _ := st.Sessions(bg, "", 10); len(ids) != 2 {
		t.Errorf("sessions after the writes = %v, want s1 and s2", ids)
	}
}

func queuedLog(t *testing.T, st harness.Store, body func(t *testing.T, live http.Handler)) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		r := runtime(t, st, f)
		s := create(t, r)
		submit(t, s, text("a", "work"))
		<-f.runs
		q := text("q1", "later")
		q.Delivery, q.Source, q.SourceID, q.SourceLabel = protocol.DeliveryQueue, "slack", "slack:C1:1", "Ann"
		q.Parts = append(q.Parts, protocol.Part{Type: protocol.PartBlob, MediaType: "image/png", Data: pngBytes(t)})
		submit(t, s, q)
		q2 := text("q2", "last")
		q2.Delivery = protocol.DeliveryQueue
		submit(t, s, q2)
		defer closeRuntime(t, r)
		body(t, r.Handler())
	})
}

func wantQueued(t *testing.T, h http.Handler, label string) {
	t.Helper()
	rec := get(t, h, "/sessions/s1/inputs")
	var got []protocol.QueuedInput
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("%s: GET inputs = %d %s (%v)", label, rec.Code, rec.Body, err)
	}
	key := "attachment-" + hex.EncodeToString(sha256Sum(pngBytes(t)))
	want := []protocol.QueuedInput{
		{ID: "q1", Delivery: "queue", Source: "slack", SourceID: "slack:C1:1", SourceLabel: "Ann", Parts: []protocol.MessagePart{
			{Type: "text", Text: "later"}, {Type: "blob", MediaType: "image/png", Bytes: len(pngBytes(t)), Key: key}}},
		{ID: "q2", Delivery: "queue", Source: "user", Parts: []protocol.MessagePart{{Type: "text", Text: "last"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: GET inputs = %+v, want %+v", label, got, want)
	}
	if strings.Contains(rec.Body.String(), `"data"`) {
		t.Errorf("%s: GET inputs carries attachment data: %s", label, rec.Body)
	}
	var v protocol.Session
	if err := json.Unmarshal(get(t, h, "/sessions/s1").Body.Bytes(), &v); err != nil || !slices.Equal(v.Queued, []string{"q1", "q2"}) {
		t.Errorf("%s: Session.queued = %v (%v), want the IDs", label, v.Queued, err)
	}
}

func sha256Sum(b []byte) []byte { s := sha256.Sum256(b); return s[:] }

func TestQueuedInputsListWithTheirPartsAndProvenanceOnBothHandlers(t *testing.T) {
	st := harness.NewMemStore()
	queuedLog(t, st, func(t *testing.T, live http.Handler) { wantQueued(t, live, "runtime handler of a running session") })
	r2 := runtime(t, st, newFake())
	t.Cleanup(func() { closeRuntime(t, r2) })
	wantQueued(t, r2.Handler(), "runtime handler of a session that it does not run")
	wantQueued(t, harness.ReadHandler(st, nil), "read handler")
}

func attachmentLog(t *testing.T, st harness.Store) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.save = "state"
		r := runtime(t, st, f)
		s := create(t, r)
		in := text("a", "look")
		in.Parts = append(in.Parts, protocol.Part{Type: protocol.PartBlob, MediaType: "image/png", Data: pngBytes(t)})
		submit(t, s, in)
		run := <-f.runs
		run.emit(say("seen"))
		run.end()
		closeRuntime(t, r)
	})
}

func wantBlobs(t *testing.T, h http.Handler, label string) {
	t.Helper()
	var page protocol.MessagePage
	if err := json.Unmarshal(get(t, h, "/sessions/s1/messages").Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	var key string
	for _, m := range page.Messages {
		for _, p := range m.Parts {
			if p.Type == protocol.MessagePartBlob {
				key = p.Key
			}
		}
	}
	if key == "" {
		t.Fatalf("%s: no blob part carries a key: %+v", label, page.Messages)
	}
	rec := get(t, h, "/sessions/s1/blobs/"+key)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" || !bytes.Equal(rec.Body.Bytes(), pngBytes(t)) {
		t.Errorf("%s: GET blob = %d %s %d bytes, want 200 image/png with the attachment", label, rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(pngBytes(t))) {
		t.Errorf("%s: GET blob Content-Length = %q, want the recorded size %d", label, got, len(pngBytes(t)))
	}
	var events protocol.EventPage
	if err := json.Unmarshal(get(t, h, "/sessions/s1/events").Body.Bytes(), &events); err != nil {
		t.Fatal(err)
	}
	var state string
	for _, e := range events.Events {
		if e.Kind == "backend.state" {
			var d struct {
				Chunk string `json:"chunk"`
			}
			_ = json.Unmarshal(e.Data, &d)
			state = cmp.Or(d.Chunk, state)
		}
	}
	if state == "" {
		t.Fatalf("%s: the log holds no backend.state", label)
	}
	for name, k := range map[string]string{"unknown": "attachment-0", "backend_state": state} {
		var body protocol.ErrorBody
		rec := get(t, h, "/sessions/s1/blobs/"+k)
		if err := json.Unmarshal(rec.Body.Bytes(), &body); rec.Code != http.StatusNotFound || err != nil || body.Error.Code != protocol.CodeBlobNotFound {
			t.Errorf("%s: GET blob %s = %d %s, want 404 blob_not_found", label, name, rec.Code, rec.Body)
		}
	}
	if rec := get(t, h, "/sessions/nope/blobs/"+key); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), protocol.CodeSessionNotFound) {
		t.Errorf("%s: GET blob of an unknown session = %d %s, want 404 session_not_found", label, rec.Code, rec.Body)
	}
}

func TestAHistoryAttachmentReadsBackByItsKeyOnBothHandlers(t *testing.T) {
	st := harness.NewMemStore()
	attachmentLog(t, st)
	r2 := runtime(t, st, newFake())
	t.Cleanup(func() { closeRuntime(t, r2) })
	wantBlobs(t, r2.Handler(), "runtime handler")
	wantBlobs(t, harness.ReadHandler(st, nil), "read handler")
}

type failingQueue struct{}

func (failingQueue) Queued(context.Context, string) ([]protocol.QueuedInput, error) {
	return nil, errors.New("queue backend down")
}

func TestAFailingQueueLeavesTheStoredHistoryReadable(t *testing.T) {
	st := harness.NewMemStore()
	attachmentLog(t, st)
	ro := harness.ReadHandler(st, failingQueue{})
	key := "attachment-" + hex.EncodeToString(sha256Sum(pngBytes(t)))
	for _, path := range []string{"/sessions/s1/messages", "/sessions/s1/events", "/sessions/s1/blobs/" + key} {
		if rec := get(t, ro, path); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d %s, want 200 while the queue fails", path, rec.Code, rec.Body)
		}
	}
	for _, path := range []string{"/sessions/s1", "/sessions/s1/inputs", "/sessions"} {
		if rec := get(t, ro, path); rec.Code != http.StatusInternalServerError {
			t.Errorf("GET %s = %d %s, want 500 while the queue fails", path, rec.Code, rec.Body)
		}
	}
}

type fakeQueue map[string][]protocol.QueuedInput

func (q fakeQueue) Queued(_ context.Context, session string) ([]protocol.QueuedInput, error) {
	return q[session], nil
}

func queueRow(id, text string) protocol.QueuedInput {
	return protocol.QueuedInput{ID: id, Delivery: "queue", Source: "user", SourceID: "u1", SourceLabel: "Ann",
		Parts: []protocol.MessagePart{{Type: "text", Text: text}}}
}

func inputsOf(t *testing.T, h http.Handler, path string) []protocol.QueuedInput {
	t.Helper()
	rec := get(t, h, path)
	var got []protocol.QueuedInput
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s (%v), want 200 with the inputs", path, rec.Code, rec.Body, err)
	}
	return got
}

func TestTheReadHandlerListsTheQueueAfterTheQueuedInputsOfTheLog(t *testing.T) {
	st := harness.NewMemStore()
	queuedLog(t, st, func(*testing.T, http.Handler) {})
	logged := inputsOf(t, harness.ReadHandler(st, nil), "/sessions/s1/inputs")
	if len(logged) != 2 {
		t.Fatalf("the log queues %d inputs, want 2", len(logged))
	}
	ro := harness.ReadHandler(st, fakeQueue{"s1": {queueRow("r1", "row one"), queueRow("r2", "row two")}, "s9": {queueRow("r3", "first prompt")}})
	t.Run("inputs_follow_the_log", func(t *testing.T) {
		got, want := inputsOf(t, ro, "/sessions/s1/inputs"), append(slices.Clone(logged), queueRow("r1", "row one"), queueRow("r2", "row two"))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("GET inputs = %+v, want %+v", got, want)
		}
	})
	t.Run("session_queued_lists_the_ids_in_the_same_order", func(t *testing.T) {
		var v protocol.Session
		if err := json.Unmarshal(get(t, ro, "/sessions/s1").Body.Bytes(), &v); err != nil || !slices.Equal(v.Queued, []string{"q1", "q2", "r1", "r2"}) {
			t.Errorf("GET /sessions/s1 queued = %v (%v), want q1 q2 r1 r2", v.Queued, err)
		}
		var page protocol.SessionPage
		if err := json.Unmarshal(get(t, ro, "/sessions").Body.Bytes(), &page); err != nil || len(page.Sessions) != 1 || !slices.Equal(page.Sessions[0].Queued, []string{"q1", "q2", "r1", "r2"}) {
			t.Errorf("GET /sessions = %+v (%v), want s1 with queued q1 q2 r1 r2 and no s9", page, err)
		}
	})
	t.Run("a_session_with_no_log_answers_the_inputs_from_the_queue_and_is_not_found", func(t *testing.T) {
		if got, want := inputsOf(t, ro, "/sessions/s9/inputs"), []protocol.QueuedInput{queueRow("r3", "first prompt")}; !reflect.DeepEqual(got, want) {
			t.Errorf("GET /sessions/s9/inputs = %+v, want %+v", got, want)
		}
		for _, path := range []string{"/sessions/s9", "/sessions/nope/inputs", "/sessions/s9/messages", "/sessions/s9/events"} {
			rec := get(t, ro, path)
			var body protocol.ErrorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); rec.Code != http.StatusNotFound || err != nil || body.Error.Code != protocol.CodeSessionNotFound {
				t.Errorf("GET %s = %d %s, want 404 session_not_found", path, rec.Code, rec.Body)
			}
		}
	})
	t.Run("a_nil_queue_lists_the_log_only", func(t *testing.T) {
		nilQueue := harness.ReadHandler(st, nil)
		if got := inputsOf(t, nilQueue, "/sessions/s1/inputs"); !reflect.DeepEqual(got, logged) {
			t.Errorf("GET inputs = %+v, want the log's %+v", got, logged)
		}
		var v protocol.Session
		if err := json.Unmarshal(get(t, nilQueue, "/sessions/s1").Body.Bytes(), &v); err != nil || !slices.Equal(v.Queued, []string{"q1", "q2"}) {
			t.Errorf("GET /sessions/s1 queued = %v (%v), want q1 q2", v.Queued, err)
		}
		if rec := get(t, nilQueue, "/sessions/s9/inputs"); rec.Code != http.StatusNotFound {
			t.Errorf("GET /sessions/s9/inputs = %d, want 404", rec.Code)
		}
	})
}
