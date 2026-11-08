package main

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

func TestListenerBackoffDoublesAndStartsOverAfterALongConnection(t *testing.T) {
	var b listenerBackoff
	failure := errors.New("connection reset")
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute}
	for i, w := range want {
		if got := b.next(time.Second, failure); got != w {
			t.Errorf("reconnect %d waits %v, want %v", i+1, got, w)
		}
	}
	if got := b.next(2*time.Minute, failure); got != 5*time.Second {
		t.Errorf("after a connection that outlived the cap = %v, want a fresh 5s", got)
	}
}

func TestListenerBackoffWaitsOutRateLimits(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want time.Duration
	}{
		{"server's wait", fmt.Errorf("failed to get message: %w", &scaleset.RateLimitError{StatusCode: 403, Kind: scaleset.RateLimitSecondary, RetryAfter: 3 * time.Minute}), 3 * time.Minute},
		{"a minute without a usable wait", &scaleset.RateLimitError{StatusCode: 403, Kind: scaleset.RateLimitSecondary}, time.Minute},
		{"backoff when the wait is shorter", &scaleset.RateLimitError{StatusCode: 429, Kind: scaleset.RateLimitSecondary, RetryAfter: time.Second}, 5 * time.Second},
		{"capped at an hour", &scaleset.RateLimitError{StatusCode: 403, Kind: scaleset.RateLimitPrimary, RetryAfter: 3 * time.Hour}, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b listenerBackoff
			if got := b.next(time.Second, tc.err); got != tc.want {
				t.Errorf("first reconnect waits %v, want %v", got, tc.want)
			}
		})
	}
}
