package health

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ysya/runscaler/internal/metrics"
)

// stubController implements InstanceCounter for testing.
type stubController struct {
	idle, busy int
}

func (s *stubController) InstanceCounts() (int, int) { return s.idle, s.busy }

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

func TestHealthzWithController(t *testing.T) {
	h := newTestServer()
	h.RegisterController("test-set", &stubController{idle: 2, busy: 3})

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
	h.RegisterController("test-set", &stubController{idle: 1, busy: 0})
	h.RegisterMetrics("test-set", &stubMetrics{snap: metrics.Snapshot{
		JobsStarted:    10,
		JobsCompleted:  8,
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

func TestReadyzNoControllers(t *testing.T) {
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

func TestReadyzWithController(t *testing.T) {
	h := newTestServer()
	h.RegisterController("test-set", &stubController{idle: 1, busy: 0})
	h.MarkConnected("test-set")

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

func TestReadyzDisconnectedController(t *testing.T) {
	h := newTestServer()
	h.RegisterController("test-set", &stubController{})
	h.MarkDisconnected("test-set", "connection lost")

	req := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	h.handleReadyz(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestHealthzDiskOmittedWithoutProvider(t *testing.T) {
	h := newTestServer()

	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	h.handleHealthz(w, req)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if _, ok := raw["disk"]; ok {
		t.Error(`"disk" key must be omitted from the JSON response when no provider is registered`)
	}
}

func TestHealthzDiskFromProvider(t *testing.T) {
	h := newTestServer()
	h.SetDiskProvider(func() []DiskStatus {
		return []DiskStatus{{Filesystem: "/", FreePercent: 38, FreeBytes: 100, TotalBytes: 200}}
	})

	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	h.handleHealthz(w, req)

	var resp HealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(resp.Disk) != 1 {
		t.Fatalf("disk length = %d, want 1", len(resp.Disk))
	}
	got := resp.Disk[0]
	want := DiskStatus{Filesystem: "/", FreePercent: 38, FreeBytes: 100, TotalBytes: 200}
	if got != want {
		t.Errorf("disk[0] = %+v, want %+v", got, want)
	}
}

func TestUnregisterController(t *testing.T) {
	h := newTestServer()
	h.RegisterController("test-set", &stubController{idle: 1, busy: 0})
	h.RegisterMetrics("test-set", &stubMetrics{})
	h.UnregisterController("test-set")

	// After unregister, readyz should return 503
	req := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	h.handleReadyz(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d after unregister", w.Code, http.StatusServiceUnavailable)
	}
}
