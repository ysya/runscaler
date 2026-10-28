package health

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ysya/runscaler/internal/metrics"
)

type runtimeController struct {
	stubController
	runtime metrics.RuntimeStatus
}

func (s *runtimeController) RuntimeStatus() metrics.RuntimeStatus { return s.runtime }

func TestRuntimeHealthAndReadiness(t *testing.T) {
	for _, tc := range []struct {
		name      string
		runtime   metrics.RuntimeStatus
		wantReady bool
	}{
		{"startup failure", metrics.RuntimeStatus{Degraded: true, StartFailures: 1, ConsecutiveStartFailures: 1, FailureReason: "provider_start_failed"}, false},
		{"capacity waiting", metrics.RuntimeStatus{WaitingForCapacity: true}, true},
		{"scale to zero", metrics.RuntimeStatus{}, true},
		{"draining", metrics.RuntimeStatus{Draining: true}, false},
		{"historical failure without unmet demand", metrics.RuntimeStatus{StartFailures: 1, ConsecutiveStartFailures: 1, FailureReason: "provider_start_failed"}, true},
		{"recovered", metrics.RuntimeStatus{StartAttempts: 2, StartFailures: 1, LastSuccessfulStart: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestServer()
			h.RegisterController("builders", &runtimeController{runtime: tc.runtime})
			h.MarkConnected("builders")
			w := httptest.NewRecorder()
			h.handleHealthz(w, httptest.NewRequest("GET", "/healthz", nil))
			if w.Code != http.StatusOK {
				t.Fatalf("liveness status = %d, want 200", w.Code)
			}
			var got HealthResponse
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got.ScaleSets) != 1 || got.ScaleSets[0].Runtime == nil || got.ScaleSets[0].Ready != tc.wantReady {
				t.Fatalf("runtime health = %s", w.Body.String())
			}
			if got.ScaleSets[0].Runtime.StartFailures != tc.runtime.StartFailures || !got.ScaleSets[0].Runtime.LastSuccessfulStart.Equal(tc.runtime.LastSuccessfulStart) {
				t.Fatalf("runtime counters or timestamps lost in JSON: %s", w.Body.String())
			}
			if strings.Contains(w.Body.String(), "0001-01-01") {
				t.Fatalf("health fabricated an unreported startup timestamp: %s", w.Body.String())
			}
			if (got.Status == "ok") != tc.wantReady {
				t.Fatalf("overall health = %q, ready=%v", got.Status, tc.wantReady)
			}
			w = httptest.NewRecorder()
			h.handleReadyz(w, httptest.NewRequest("GET", "/readyz", nil))
			wantCode := http.StatusServiceUnavailable
			if tc.wantReady {
				wantCode = http.StatusOK
			}
			if w.Code != wantCode {
				t.Fatalf("readiness = %d, want %d", w.Code, wantCode)
			}
		})
	}
}
