package session

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// blobBackend reads the blob of the first input of each turn, then ends the turn.
type blobBackend struct {
	mu   sync.Mutex
	got  []byte
	err  error
	read bool
}

func (*blobBackend) Capabilities(string) turn.Capabilities { return turn.Capabilities{} }

func (b *blobBackend) Run(ctx context.Context, req turn.Request, out turn.Sink) (turn.Result, error) {
	b.mu.Lock()
	b.read = req.Blob != nil
	if b.read {
		b.got, b.err = req.Blob(ctx, req.Input[0].Parts[1].BlobKey)
	}
	b.mu.Unlock()
	return turn.Result{}, out.Item(eventlog.Message{Role: eventlog.RoleAssistant, Parts: []eventlog.Part{{Type: eventlog.PartText, Text: "ok"}}})
}

// recordSync acknowledges each batch and keeps it.
type recordSync struct {
	mu      sync.Mutex
	batches []protocol.SyncBatch
}

func (s *recordSync) Deliver(_ context.Context, b protocol.SyncBatch) (protocol.SyncAck, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches = append(s.batches, b)
	return protocol.SyncAck{Head: b.FromSeq + uint64(len(b.Records)) - 1}, nil
}

func attachment(key string) *eventlog.InputAdmitted {
	return &eventlog.InputAdmitted{InputID: "in1", Delivery: eventlog.DeliveryQueue, Source: "user", Parts: []eventlog.Part{
		{Type: eventlog.PartText, Text: "look"}, {Type: eventlog.PartBlob, MediaType: "image/png", BlobKey: key, Bytes: 3}}}
}

func TestATurnReadsTheBlobOfItsInputAndSyncCarriesIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		log, b, sync := &memLog{blobs: map[string][]byte{"k": []byte("png")}}, &blobBackend{}, &recordSync{}
		cfg := actorConfig(t, log, owned{}, b)
		cfg.Sync = sync
		a, err := Create(context.Background(), cfg, eventlog.SessionCreated{Model: "m/m"}, attachment("k"))
		if err != nil {
			t.Fatal(err)
		}
		a.Run()
		synctest.Wait()
		if !b.read || b.err != nil || !bytes.Equal(b.got, []byte("png")) {
			t.Errorf("Request.Blob = %q, %v (set %t), want the bytes of the blob", b.got, b.err, b.read)
		}
		var carried []byte
		for _, batch := range sync.batches {
			if v, ok := batch.Blobs["k"]; ok {
				carried = v
			}
		}
		if !bytes.Equal(carried, []byte("png")) {
			t.Errorf("a SyncBatch carries the blob %q, want png", carried)
		}
	})
}
