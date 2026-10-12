package provider

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/majorcontext/harness/internal/backoff"
)

// ParseRetryAfter reads the Retry-After header of h, a number of seconds or an
// HTTP date. ok is false when h has no valid value. A date in the past is a wait of 0.
func ParseRetryAfter(h http.Header, now time.Time) (d time.Duration, ok bool) {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs * float64(time.Second)), true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0), true
	}
	return 0, false
}

// Do sends req on hc. It sends the request again, at most retries times, after
// a 5xx status or a transport error, and waits for the Retry-After header of
// the response, or else backoff.Delay. A 429 is not sent again. The last response or
// error is the result. A transport error that is not the end of ctx is retryable
// with class RetryableConnectionFailed. req needs a GetBody for a body.
func Do(ctx context.Context, hc *http.Client, req *http.Request, retries int) (*http.Response, error) {
	for n := 1; ; n++ {
		resp, err := hc.Do(req)
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, err
		case err != nil && n > retries:
			return nil, MarkRetryable(err, RetryableConnectionFailed)
		case err == nil && (resp.StatusCode < 500 || n > retries):
			return resp, nil
		}
		wait := backoff.Delay(n)
		if err == nil {
			if after, ok := ParseRetryAfter(resp.Header, time.Now()); ok {
				wait = after
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			_ = resp.Body.Close()
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return nil, context.Cause(ctx)
		}
		if req.GetBody != nil {
			body, gerr := req.GetBody()
			if gerr != nil {
				return nil, gerr
			}
			req = req.Clone(ctx)
			req.Body = body
		}
	}
}
