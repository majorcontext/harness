package openai

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

const codexZstdEncoderCapacity = 4

// zstdRequestEncoderPool bounds aggregate encoder memory and lets a canceled
// HTTP fallback stop while it waits for compression capacity.
type zstdRequestEncoderPool struct {
	capacity int
	once     sync.Once
	slots    chan struct{}
	encoders sync.Pool
}

func (p *zstdRequestEncoderPool) initialize() {
	p.once.Do(func() {
		capacity := p.capacity
		if capacity < 1 {
			capacity = 1
		}
		p.slots = make(chan struct{}, capacity)
	})
}

func (p *zstdRequestEncoderPool) compress(ctx context.Context, body []byte) ([]byte, error) {
	p.initialize()
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	pooled, _ := p.encoders.Get().(*zstd.Encoder)
	if pooled == nil {
		var err error
		pooled, err = zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)),
			zstd.WithEncoderConcurrency(1),
		)
		if err != nil {
			return nil, fmt.Errorf("openai: initialize zstd request encoder: %w", err)
		}
	}
	defer p.encoders.Put(pooled)
	compressed := pooled.EncodeAll(body, nil)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return compressed, nil
}

var codexZstdEncoders = zstdRequestEncoderPool{capacity: codexZstdEncoderCapacity}

func compressCodexHTTPRequest(ctx context.Context, body []byte) ([]byte, error) {
	started := time.Now()
	compressed, err := codexZstdEncoders.compress(ctx, body)
	if err != nil {
		return nil, err
	}
	slog.Debug("openai: compressed request body with zstd",
		"before_bytes", len(body),
		"after_bytes", len(compressed),
		"duration_ms", time.Since(started).Milliseconds(),
	)
	return compressed, nil
}
