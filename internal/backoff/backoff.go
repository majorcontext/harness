// Package backoff holds the wait between two attempts of a retried model request or model call.
package backoff

import (
	"math/rand/v2"
	"time"
)

const base = 200 * time.Millisecond

// Delay returns the wait before retry number n, where the first retry is 1: 200 ms that doubles with each retry, with up to 10% jitter either way.
func Delay(n int) time.Duration {
	d := base << min(max(n-1, 0), 30)
	return time.Duration(float64(d) * (0.9 + 0.2*rand.Float64()))
}
