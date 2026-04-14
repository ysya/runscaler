package health

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ysya/runscaler/internal/metrics"
)

// stubScaler implements RunnerCounter for testing.
type stubScaler struct {
	idle, busy int
}

func (s *stubScaler) RunnerCounts() (int, int) { return s.idle, s.busy }

// stubMetrics implements MetricsProvider for testing.
type stubMetrics struct {
	snap metrics.Snapshot
}

func (s *stubMetrics) Snapshot() metrics.Snapshot { return s.snap }

func newTestServer() *HealthServer {
	return NewHealthServer(0, "test-version", slog.Default())
}

func TestHealthzEmpty(t *testing.T) {
	h := newTestServer()

	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	h.handleHealthz(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp HealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("status = %q, want %q", resp.Status, "ok")
	}
	if resp.Version != "test-version" {
		t.Errorf("version = %q, want %q", resp.Version, "test-version")
	}
	if len(resp.ScaleSets) != 0 {
		t.Errorf("scale_sets length = %d, want 0", len(resp.ScaleSets))
	}
}

func TestHealthzWithScaler(t *testing.T) {
	h := newTestServer()
	h.RegisterScaler("test-set", &stubScaler{idle: 2, busy: 3})

	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	h.handleHealthz(w, req)

	var resp HealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(resp.ScaleSets) != 1 {
		t.Fatalf("scale_sets length = %d, want 1", len(resp.ScaleSets))
	}
	ss := resp.ScaleSets[0]
	if ss.Name != "test-set" {
		t.Errorf("name = %q, want %q", ss.Name, "test-set")
	}
	if ss.Idle != 2 || ss.Busy != 3 {
		t.Errorf("idle/busy = %d/%d, want 2/3", ss.Idle, ss.Busy)
	}
}

func TestHealthzWithMetrics(t *testing.T) {
	h := newTestServer()
	h.RegisterScaler("test-set", &stubScaler{idle: 1, busy: 0})
	h.RegisterMetrics("test-set", &stubMetrics{snap: metrics.Snapshot{
		JobsStarted:   10,
		JobsCompleted: 8,
		DesiredRunners: 2,
	}})

	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	h.handleHealthz(w, req)

	var resp HealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	ss := resp.ScaleSets[0]
	if ss.Metrics == nil {
		t.Fatal("metrics is nil")
	}
	if ss.Metrics.JobsStarted != 10 {
		t.Errorf("jobs_started = %d, want 10", ss.Metrics.JobsStarted)
	}
	if ss.Metrics.JobsCompleted != 8 {
		t.Errorf("jobs_completed = %d, want 8", ss.Metrics.JobsCompleted)
	}
}

func TestReadyzNoScalers(t *testing.T) {
	h := newTestServer()

	req := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	h.handleReadyz(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["status"] != "not ready" {
		t.Errorf("status = %q, want %q", resp["status"], "not ready")
	}
}

func TestReadyzWithScaler(t *testing.T) {
	h := newTestServer()
	h.RegisterScaler("test-set", &stubScaler{idle: 1, busy: 0})

	req := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	h.handleReadyz(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %q, want %q", resp["status"], "ok")
	}
}

func TestUnregisterScaler(t *testing.T) {
	h := newTestServer()
	h.RegisterScaler("test-set", &stubScaler{idle: 1, busy: 0})
	h.RegisterMetrics("test-set", &stubMetrics{})
	h.UnregisterScaler("test-set")

	// After unregister, readyz should return 503
	req := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	h.handleReadyz(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d after unregister", w.Code, http.StatusServiceUnavailable)
	}
}
