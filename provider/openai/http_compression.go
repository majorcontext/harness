package openai

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// codexZstdEncoderPool creates level-3 encoders only when a Codex HTTP request
// needs one. Each encoder processes one request at a time; the pool permits
// independent sessions to compress concurrently without adding startup work.
var codexZstdEncoderPool = sync.Pool{
	New: func() any {
		encoder, err := zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)),
			zstd.WithEncoderConcurrency(1),
		)
		return &codexZstdEncoder{encoder: encoder, err: err}
	},
}

type codexZstdEncoder struct {
	encoder *zstd.Encoder
	err     error
}

func compressCodexHTTPRequest(body []byte) ([]byte, error) {
	pooled := codexZstdEncoderPool.Get().(*codexZstdEncoder)
	if pooled.err != nil {
		codexZstdEncoderPool.Put(pooled)
		return nil, fmt.Errorf("openai: initialize zstd request encoder: %w", pooled.err)
	}
	started := time.Now()
	compressed := pooled.encoder.EncodeAll(body, nil)
	codexZstdEncoderPool.Put(pooled)
	slog.Debug("openai: compressed request body with zstd",
		"before_bytes", len(body),
		"after_bytes", len(compressed),
		"duration_ms", time.Since(started).Milliseconds(),
	)
	return compressed, nil
}
