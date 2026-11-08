package main

import (
	"testing"
	"time"
)

func TestListenerBackoffDoublesAndStartsOverAfterALongConnection(t *testing.T) {
	var b listenerBackoff
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute}
	for i, w := range want {
		if got := b.next(time.Second); got != w {
			t.Errorf("reconnect %d waits %v, want %v", i+1, got, w)
		}
	}
	if got := b.next(2 * time.Minute); got != 5*time.Second {
		t.Errorf("after a connection that outlived the cap = %v, want a fresh 5s", got)
	}
}
