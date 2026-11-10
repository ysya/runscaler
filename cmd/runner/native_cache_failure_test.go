package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNativeCacheFailureCancelsRunnerDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failures := make(chan error, 1)
	errs := make(chan error, 1)
	done := watchNativeCacheFailure(ctx, failures, cancel, errs)
	failures <- errors.New("listener failed")
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("runner context not cancelled")
	}
	select {
	case err := <-errs:
		if !strings.Contains(err.Error(), "native cache listener") {
			t.Fatalf("wrong failure: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cache failure not reported")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("failure watcher did not exit")
	}
}
