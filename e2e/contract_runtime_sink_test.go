package e2e

import (
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/testpoll"
)

const sinkGeneration = "jrnl_contract"

var oneTurnTypes = "session.created session.status request.meta message message turn.end session.status"

func sinkConfig(rcv *harnesstest.SinkReceiver, extra map[string]any) map[string]any {
	sink := map[string]any{
		"url":        rcv.URL(),
		"generation": sinkGeneration,
		"flush_ms":   20,
		"headers":    map[string]string{"X-Contract": "1"},
	}
	for k, v := range extra {
		sink[k] = v
	}
	return map[string]any{"event_sink": sink}
}

func replyText(text string) harnesstest.Step {
	return harnesstest.Step{Name: "reply", Reply: harnesstest.Reply{Text: text}, Repeat: true}
}

func awaitSinkThrough(t *testing.T, d *httpDriver, rcv *harnesstest.SinkReceiver, seq int64) {
	t.Helper()
	ok := rcv.Await(func(bs []harnesstest.SinkBatch) bool {
		return slices.ContainsFunc(bs, func(b harnesstest.SinkBatch) bool { return b.Applied >= seq })
	}, waitBound)
	if !ok {
		t.Fatalf("receiver never acknowledged through seq %d; batches: %s\nserve stderr:\n%s", seq, sinkSummary(rcv.Batches()), d.Stderr())
	}
}

func sinkSummary(batches []harnesstest.SinkBatch) string {
	parts := make([]string, len(batches))
	for i, b := range batches {
		parts[i] = fmt.Sprintf("[%d..%d n=%d status=%d applied=%d]", b.FromSeq, b.ToSeq, len(b.Records), b.Status, b.Applied)
	}
	return strings.Join(parts, " ")
}

func flatRecords(batches []harnesstest.SinkBatch) []harnesstest.SinkRecord {
	var out []harnesstest.SinkRecord
	for _, b := range batches {
		out = append(out, b.Records...)
	}
	return out
}

func recordTypes(recs []harnesstest.SinkRecord) string {
	types := make([]string, len(recs))
	for i, r := range recs {
		types[i] = r.Type
	}
	return strings.Join(types, " ")
}

func recordSeqs(recs []harnesstest.SinkRecord) []int64 {
	seqs := make([]int64, len(recs))
	for i, r := range recs {
		seqs[i] = r.Seq
	}
	return seqs
}

func seqRange(from, to int64) []int64 {
	var out []int64
	for s := from; s <= to; s++ {
		out = append(out, s)
	}
	return out
}

// checkBatchChain fails unless every batch starts right after the previous one ended.
func checkBatchChain(t *testing.T, batches []harnesstest.SinkBatch) {
	t.Helper()
	next := int64(1)
	for i, b := range batches {
		if b.FromSeq != next {
			t.Errorf("batch %d starts at seq %d, want %d", i, b.FromSeq, next)
		}
		next = b.ToSeq + 1
	}
}

func TestContractRuntimeEventSink(t *testing.T) {
	skipShort(t)
	rows := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"event_sink_ships_every_durable_record", sinkShipsEveryRecord},
		{"event_sink_include_types_ships_checkpoints", sinkIncludeTypes},
		{"event_sink_receiver_rewind_reships_from_the_start", sinkRewind},
		{"event_sink_resumes_from_receiver_cursor_after_restart", sinkRestart},
		{"event_sink_retries_a_retryable_failure", sinkRetries},
		{"event_sink_permanent_rejection_retires_only_the_pump", sinkPermanentRejection},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			row.run(t)
		})
	}
}

func sinkShipsEveryRecord(t *testing.T) {
	rcv := harnesstest.NewSinkReceiver(t, nil)
	d, _ := startRuntime(t, runtimeWorkdir(t, nil), sinkConfig(rcv, nil), replyText("hi"))
	runTurn(t, d, "hello")
	tip := eventTip(t, d)
	awaitSinkThrough(t, d, rcv, tip)

	batches := rcv.Batches()
	checkBatchChain(t, batches)
	recs := flatRecords(batches)
	if got := recordTypes(recs); got != oneTurnTypes {
		t.Errorf("record types = %q, want %q (live-only deltas stay out)", got, oneTurnTypes)
	}
	if got, want := recordSeqs(recs), seqRange(1, tip); !slices.Equal(got, want) {
		t.Errorf("record seqs = %v, want %v", got, want)
	}
	for i, b := range batches {
		if b.Generation != sinkGeneration || b.Header.Get("X-Contract") != "1" {
			t.Errorf("batch %d generation %q header %q, want %q and the configured header", i, b.Generation, b.Header.Get("X-Contract"), sinkGeneration)
		}
		if b.Filtered || strings.Contains(b.Body, `"filtered"`) {
			t.Errorf("batch %d of an unfiltered sink carries a filtered key: %s", i, b.Body[:min(len(b.Body), 80)])
		}
	}
	for _, r := range recs {
		if r.RecordedAt == "" {
			t.Errorf("record %d (%s) has no recorded_at", r.Seq, r.Type)
		}
	}
}

func sinkIncludeTypes(t *testing.T) {
	rcv := harnesstest.NewSinkReceiver(t, nil)
	cfg := sinkConfig(rcv, map[string]any{"include_types": []string{"message", "turn.end"}, "batch_max_records": 2})
	d, _ := startRuntime(t, runtimeWorkdir(t, nil), cfg, replyText("hi"))
	runTurn(t, d, "hello")
	tip := eventTip(t, d)
	awaitSinkThrough(t, d, rcv, tip)

	batches := rcv.Batches()
	checkBatchChain(t, batches)
	first := batches[0]
	if first.RecordsJSON != "[]" || first.FromSeq != 1 {
		t.Errorf("first batch = from %d records %s, want an empty checkpoint from seq 1 that encodes []", first.FromSeq, first.RecordsJSON)
	}
	for i, b := range batches {
		if !b.Filtered || !strings.Contains(b.Body, `"filtered":true`) {
			t.Errorf("batch %d does not report filtered: %s", i, b.Body[:min(len(b.Body), 80)])
		}
	}
	recs := flatRecords(batches)
	if got, want := recordTypes(recs), "message message turn.end"; got != want {
		t.Errorf("selected record types = %q, want %q", got, want)
	}
	if got, want := recordSeqs(recs), []int64{4, 5, 6}; !slices.Equal(got, want) {
		t.Errorf("selected record seqs = %v, want %v", got, want)
	}
	if last := batches[len(batches)-1]; last.ToSeq != tip {
		t.Errorf("last batch ends at seq %d, want the tip %d: the cursor crosses unselected records", last.ToSeq, tip)
	}
}

func sinkRewind(t *testing.T) {
	var gate atomic.Int64
	gate.Store(1 << 60)
	var rewound atomic.Bool
	rcv := harnesstest.NewSinkReceiver(t, func(_ int, b harnesstest.SinkBatch) harnesstest.SinkReply {
		if b.FromSeq > gate.Load() && rewound.CompareAndSwap(false, true) {
			return harnesstest.SinkReply{AppliedThrough: 0}
		}
		return harnesstest.SinkReply{AppliedThrough: b.ToSeq}
	})
	d, _ := startRuntime(t, runtimeWorkdir(t, nil), sinkConfig(rcv, nil), replyText("hi"))
	id := runTurn(t, d, "one")
	tip1 := eventTip(t, d)
	awaitSinkThrough(t, d, rcv, tip1)

	gate.Store(tip1)
	d.Submit(t, id, "two")
	d.WaitIdle(t, id)
	tip2 := eventTip(t, d)
	rewindAt := func(bs []harnesstest.SinkBatch) int {
		return slices.IndexFunc(bs, func(b harnesstest.SinkBatch) bool { return b.FromSeq > tip1 })
	}
	awaitSinkThrough(t, d, rcv, tip2)
	ok := rcv.Await(func(bs []harnesstest.SinkBatch) bool {
		i := rewindAt(bs)
		return i >= 0 && len(bs) > i+1 && bs[len(bs)-1].ToSeq >= tip2
	}, waitBound)
	if !ok {
		t.Fatalf("receiver saw no re-ship after answering 0; batches: %s\nstderr:\n%s", sinkSummary(rcv.Batches()), d.Stderr())
	}
	batches := rcv.Batches()
	after := batches[rewindAt(batches)+1:]
	if after[0].FromSeq != 1 {
		t.Errorf("batch after the rewind starts at seq %d, want 1", after[0].FromSeq)
	}
	if got, want := recordSeqs(flatRecords(after)), seqRange(1, tip2); !slices.Equal(got, want) {
		t.Errorf("re-shipped seqs = %v, want %v", got, want)
	}
}

func sinkRestart(t *testing.T) {
	var applied atomic.Int64
	rcv := harnesstest.NewSinkReceiver(t, func(_ int, b harnesstest.SinkBatch) harnesstest.SinkReply {
		if b.FromSeq <= applied.Load()+1 && b.ToSeq > applied.Load() {
			applied.Store(b.ToSeq)
		}
		return harnesstest.SinkReply{AppliedThrough: applied.Load()}
	})
	cfg := sinkConfig(rcv, nil)
	cfg["event_sink"].(map[string]any)["headers"] = map[string]string{"X-Epoch": "1"}
	d, _ := startRuntime(t, runtimeWorkdir(t, nil), cfg, replyText("hi"))
	id := runTurn(t, d, "one")
	tip1 := eventTip(t, d)
	awaitSinkThrough(t, d, rcv, tip1)

	patchConfig(t, d.config, func(c map[string]any) {
		c["event_sink"].(map[string]any)["headers"] = map[string]string{"X-Epoch": "2"}
	})
	d.Restart(t, false)
	afterRestart := func(bs []harnesstest.SinkBatch) []harnesstest.SinkBatch {
		return slices.DeleteFunc(slices.Clone(bs), func(b harnesstest.SinkBatch) bool { return b.Header.Get("X-Epoch") != "2" })
	}
	if !rcv.Await(func(bs []harnesstest.SinkBatch) bool { return len(afterRestart(bs)) > 0 }, waitBound) {
		t.Fatalf("restarted process shipped nothing; batches: %s\nstderr:\n%s", sinkSummary(rcv.Batches()), d.Stderr())
	}
	d.Submit(t, id, "two")
	d.WaitIdle(t, id)
	tip2 := eventTip(t, d)
	awaitSinkThrough(t, d, rcv, tip2)

	batches := afterRestart(rcv.Batches())
	if batches[0].FromSeq != 1 || batches[0].ToSeq < tip1 {
		t.Errorf("first batch after the restart = %d..%d, want the restored journal 1..%d shipped without a new record", batches[0].FromSeq, batches[0].ToSeq, tip1)
	}
	for i, b := range batches[1:] {
		if b.FromSeq <= tip1 {
			t.Errorf("batch %d after the replay starts at seq %d, want only records above the receiver cursor %d; batches: %s", i+1, b.FromSeq, tip1, sinkSummary(batches))
		}
	}
	if last := batches[len(batches)-1]; last.ToSeq != tip2 {
		t.Errorf("batches after the restart end at seq %d, want the tip %d", last.ToSeq, tip2)
	}
}

func sinkRetries(t *testing.T) {
	rcv := harnesstest.NewSinkReceiver(t, func(n int, b harnesstest.SinkBatch) harnesstest.SinkReply {
		if n == 1 {
			return harnesstest.SinkReply{Status: 503}
		}
		return harnesstest.SinkReply{AppliedThrough: b.ToSeq}
	})
	d, _ := startRuntime(t, runtimeWorkdir(t, nil), sinkConfig(rcv, nil), replyText("hi"))
	runTurn(t, d, "hello")
	tip := eventTip(t, d)
	awaitSinkThrough(t, d, rcv, tip)

	batches := rcv.Batches()
	if len(batches) < 2 || batches[1].FromSeq != batches[0].FromSeq {
		t.Fatalf("batches = %s, want the failed batch sent again from the same seq", sinkSummary(batches))
	}
	if got, want := recordSeqs(flatRecords(batches[1:])), seqRange(1, tip); !slices.Equal(got, want) {
		t.Errorf("records after the failure = %v, want every record %v", got, want)
	}
}

func sinkPermanentRejection(t *testing.T) {
	rcv := harnesstest.NewSinkReceiver(t, func(int, harnesstest.SinkBatch) harnesstest.SinkReply {
		return harnesstest.SinkReply{Status: 400}
	})
	d, _ := startRuntime(t, runtimeWorkdir(t, nil), sinkConfig(rcv, nil), replyText("hi"))
	id := runTurn(t, d, "one")
	const stopped = "event sink stopped: receiver rejected the batch"
	testpoll.Until(t, waitBound, "serve never logged the permanent rejection", func() bool {
		return strings.Contains(d.Stderr(), stopped)
	})

	d.Submit(t, id, "two")
	d.WaitIdle(t, id)
	if got := d.GetSession(t, id).Status; got != 200 {
		t.Errorf("GET session after the rejection = %d, want 200", got)
	}
	d.p.terminate(t)
	if got := len(rcv.Batches()); got != 1 {
		t.Errorf("receiver saw %d batches, want exactly 1: a permanent rejection is not retried", got)
	}
	if got := strings.Count(d.Stderr(), stopped); got != 1 {
		t.Errorf("serve logged the rejection %d times, want once", got)
	}
}
