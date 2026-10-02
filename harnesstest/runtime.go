package harnesstest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// SinkRecord is one durable journal record in a delivered batch.
type SinkRecord struct {
	Seq        int64  `json:"seq"`
	Type       string `json:"type"`
	RecordedAt string `json:"recorded_at"`
}

// SinkBatch is one request that harness sent to an event-sink receiver.
type SinkBatch struct {
	Generation string
	FromSeq    int64
	ToSeq      int64
	Filtered   bool
	Records    []SinkRecord
	// RecordsJSON is the raw "records" value, so a test can tell [] from null.
	RecordsJSON string
	// Body is the raw request body, so a test can check which keys are present.
	Body   string
	Header http.Header
	// Status is the HTTP status the receiver answered, and Applied the
	// cursor it reported. Applied is zero for a non-2xx status.
	Status  int
	Applied int64
}

// SinkReply is how a SinkReceiver answers one batch.
type SinkReply struct {
	// Status is the HTTP status. Zero means 200.
	Status int
	// AppliedThrough is the cursor a 2xx reply reports.
	AppliedThrough int64
}

// SinkReceiver is a scripted event-sink receiver that records every batch.
type SinkReceiver struct {
	srv     *httptest.Server
	closing chan struct{}
	reply   func(n int, b SinkBatch) SinkReply

	replyMu sync.Mutex // serializes reply calls so n follows arrival order
	mu      sync.Mutex
	batches []SinkBatch
	arrived chan struct{} // closed and replaced when a batch is recorded
}

// NewSinkReceiver starts a receiver; reply gets the 1-based request number, nil acks through ToSeq.
func NewSinkReceiver(t testing.TB, reply func(n int, b SinkBatch) SinkReply) *SinkReceiver {
	t.Helper()
	r := &SinkReceiver{closing: make(chan struct{}), reply: reply, arrived: make(chan struct{})}
	r.srv = httptest.NewServer(http.HandlerFunc(r.handle))
	t.Cleanup(func() {
		close(r.closing)
		r.srv.Close()
	})
	return r
}

// URL is the URL to configure as event_sink.url.
func (r *SinkReceiver) URL() string { return r.srv.URL }

// Batches returns a copy of every batch received so far.
func (r *SinkReceiver) Batches() []SinkBatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]SinkBatch(nil), r.batches...)
}

// Await re-checks cond on each arrival until it holds or bound passes; cond must not keep or modify its argument.
func (r *SinkReceiver) Await(cond func([]SinkBatch) bool, bound time.Duration) bool {
	timer := time.NewTimer(bound)
	defer timer.Stop()
	for {
		r.mu.Lock()
		ok, arrived := cond(r.batches), r.arrived
		r.mu.Unlock()
		if ok {
			return true
		}
		select {
		case <-arrived:
		case <-timer.C:
			return false
		case <-r.closing:
			return false
		}
	}
}

func (r *SinkReceiver) handle(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var wire struct {
		Generation string          `json:"generation"`
		FromSeq    int64           `json:"from_seq"`
		ToSeq      int64           `json:"to_seq"`
		Filtered   bool            `json:"filtered"`
		Records    json.RawMessage `json:"records"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	b := SinkBatch{
		Generation: wire.Generation, FromSeq: wire.FromSeq, ToSeq: wire.ToSeq, Filtered: wire.Filtered,
		RecordsJSON: string(wire.Records), Body: string(body), Header: req.Header.Clone(),
	}
	if err := json.Unmarshal(wire.Records, &b.Records); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	r.replyMu.Lock()
	defer r.replyMu.Unlock()
	r.mu.Lock()
	n := len(r.batches) + 1
	r.mu.Unlock()

	rep := SinkReply{AppliedThrough: b.ToSeq}
	if r.reply != nil {
		rep = r.reply(n, b)
	}
	b.Status = http.StatusOK
	if rep.Status != 0 {
		b.Status = rep.Status
	}
	if b.Status/100 == 2 {
		b.Applied = rep.AppliedThrough
	}

	r.mu.Lock()
	r.batches = append(r.batches, b)
	close(r.arrived)
	r.arrived = make(chan struct{})
	r.mu.Unlock()

	if b.Status/100 != 2 {
		w.WriteHeader(b.Status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int64{"applied_through": b.Applied})
}
