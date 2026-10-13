package provider

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/majorcontext/harness/internal/backoff"
)

// ParseRetryAfter reads the Retry-After header of h: a number of seconds in
// ASCII digits, or an HTTP date. ok is false when h has no valid value. A date in
// the past is a wait of 0.
func ParseRetryAfter(h http.Header, now time.Time) (d time.Duration, ok bool) {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v != "" && strings.Trim(v, "0123456789") == "" {
		secs, err := strconv.ParseInt(v, 10, 64)
		if err != nil || secs > int64(math.MaxInt64/time.Second) {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0), true
	}
	return 0, false
}

// permanentDeclarer is implemented by a transport error that no retry can
// resolve. errors.As stops at the first error in the chain that has a
// Permanent method, so a wrapper that has one decides for the whole chain.
type permanentDeclarer interface{ Permanent() bool }

func declaresPermanent(err error) bool {
	var p permanentDeclarer
	return errors.As(err, &p) && p.Permanent()
}

// Do sends req on hc. It sends the request again, at most retries times, after
// a 5xx status or a transport error, and waits backoff.Delay. A 429 is not sent
// again, and neither is a 5xx with a Retry-After header: the turn waits for that
// advice. The last response or error is the result. A transport error that is
// not the end of ctx is retryable with class RetryableConnectionFailed, unless
// the error declares itself permanent through a Permanent() bool method that
// reports true: Do returns it at once, marked with MarkPermanent. req
// needs a GetBody for a body.
func Do(ctx context.Context, hc *http.Client, req *http.Request, retries int) (*http.Response, error) {
	for n := 1; ; n++ {
		resp, err := hc.Do(req)
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, err
		case err != nil && declaresPermanent(err):
			return nil, MarkPermanent(err)
		case err != nil && n > retries:
			return nil, MarkRetryable(err, RetryableConnectionFailed)
		case err == nil && (resp.StatusCode < 500 || n > retries || advised(resp)):
			return resp, nil
		}
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			_ = resp.Body.Close()
		}
		t := time.NewTimer(backoff.Delay(n))
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

func advised(resp *http.Response) bool {
	_, ok := ParseRetryAfter(resp.Header, time.Now())
	return ok
}
