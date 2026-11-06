package main

import (
	"strings"
	"testing"
	"time"

	"github.com/ysya/runscaler/internal/health"
	"github.com/ysya/runscaler/internal/metrics"
)

func TestStatusExplainsRuntimeSupplyState(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		runtime     metrics.RuntimeStatus
		ready       bool
	}{
		{"waiting", "WAITING FOR CAPACITY", metrics.RuntimeStatus{WaitingForCapacity: true}, true},
		{"draining", "DRAINING", metrics.RuntimeStatus{Draining: true}, false},
		{"failed", "ERROR", metrics.RuntimeStatus{Degraded: true, StartAttempts: 3, StartFailures: 3, ConsecutiveStartFailures: 3, FailureReason: "jit_config_failed"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ss := health.ScaleSetStatus{Name: "builders", Ready: tc.ready, Runtime: &tc.runtime}
			got := formatStatus(health.HealthResponse{Status: "ok", ScaleSets: []health.ScaleSetStatus{ss}}, "endpoint", time.Now(), false)
			if !strings.Contains(got, tc.state) {
				t.Fatalf("runtime state missing from status: %s", got)
			}
			if tc.runtime.ConsecutiveStartFailures > 0 && (!strings.Contains(got, "3 failed") || !strings.Contains(got, "jit_config_failed")) {
				t.Fatalf("startup failure details missing: %s", got)
			}
		})
	}
}
