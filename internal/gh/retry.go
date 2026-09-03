package gh

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/cenkalti/backoff/v5"
)

// waitOut runs an operation, waiting out the rate limits GitHub answers with
// instead of failing on them.
//
// Both halves of this package need it and for a while both had their own: the
// API client wrapping go-github's typed errors, the session client reading
// Retry-After off a form post. They had drifted to different curves and
// different attempt caps, and neither jittered — which matters most exactly
// when it is needed, because a run of thousands of writes that all back off on
// the same fixed schedule marches in step with the window it is waiting for.
//
// An operation signals what it wants by the error it returns: backoff.Permanent
// to stop, backoff.RetryAfter to ask for a particular delay, anything else to
// be retried on the curve.
func waitOut[T any](ctx context.Context, log func(string, ...any), op func() (T, error)) (T, error) {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = 2 * time.Second
	b.MaxInterval = 2 * time.Minute
	// RandomizationFactor defaults to 0.5, which is the jitter.

	return backoff.Retry(ctx, op,
		backoff.WithBackOff(b),
		// A budget rather than a number of attempts: what matters is not
		// spending an unbounded part of a long run inside one request.
		backoff.WithMaxElapsedTime(30*time.Minute),
		backoff.WithNotify(func(err error, d time.Duration) {
			if log != nil {
				log("rate limited, waiting %s", d.Round(time.Second))
			}
		}),
	)
}

// throttle recognises GitHub asking for a slower pace and turns it into a wait
// the retry loop honours.
//
// Secondary rate limits arrive as 429, or as 403 carrying Retry-After. Without
// a Retry-After the documentation says to wait at least a minute, which is far
// longer than the exponential curve would start at, so it is stated explicitly
// rather than left to the backoff.
func throttle(resp *http.Response) (error, bool) {
	limited := resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden && resp.Header.Get("Retry-After") != "")
	if !limited {
		return nil, false
	}
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return backoff.RetryAfter(secs + 1), true
		}
	}
	return backoff.RetryAfter(60), true
}
