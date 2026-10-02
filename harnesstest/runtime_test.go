package harnesstest

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func postSink(t *testing.T, r *SinkReceiver, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(r.URL(), "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func TestSinkReceiverRecordsAndAcknowledges(t *testing.T) {
	r := NewSinkReceiver(t, func(n int, b SinkBatch) SinkReply {
		if n == 2 {
			return SinkReply{Status: http.StatusServiceUnavailable}
		}
		return SinkReply{AppliedThrough: b.ToSeq}
	})
	status, body := postSink(t, r, `{"generation":"g","from_seq":1,"to_seq":3,"records":[{"seq":1,"type":"message","session_id":"s"}]}`)
	if status != http.StatusOK || strings.TrimSpace(body) != `{"applied_through":3}` {
		t.Fatalf("first reply = %d %q, want 200 applied_through 3", status, body)
	}
	if status, _ := postSink(t, r, `{"from_seq":4,"to_seq":4,"filtered":true,"records":[]}`); status != http.StatusServiceUnavailable {
		t.Fatalf("scripted failure status = %d, want 503", status)
	}
	got := r.Batches()
	if len(got) != 2 || got[0].Generation != "g" || got[0].Records[0].Type != "message" {
		t.Fatalf("batches = %+v, want the first batch decoded", got)
	}
	if got[0].Status != http.StatusOK || got[0].Applied != 3 || got[1].Status != http.StatusServiceUnavailable || got[1].Applied != 0 {
		t.Errorf("recorded replies = %d/%d and %d/%d, want 200/3 and 503/0", got[0].Status, got[0].Applied, got[1].Status, got[1].Applied)
	}
	if got[1].RecordsJSON != "[]" || !got[1].Filtered {
		t.Errorf("second batch = %+v, want filtered with records []", got[1])
	}
	if !r.Await(func(bs []SinkBatch) bool { return len(bs) == 2 }, time.Second) {
		t.Error("Await missed a condition that already holds")
	}
	if r.Await(func(bs []SinkBatch) bool { return len(bs) == 3 }, 10*time.Millisecond) {
		t.Error("Await reported a batch that never arrived")
	}
}
