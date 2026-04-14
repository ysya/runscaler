package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/ysya/runscaler/internal/metrics"
)

// RunnerCounter provides runner count information.
type RunnerCounter interface {
	RunnerCounts() (idle, busy int)
}

// MetricsProvider provides a snapshot of listener metrics for a scale set.
type MetricsProvider interface {
	Snapshot() metrics.Snapshot
}

// HealthServer provides /healthz and /readyz endpoints for monitoring.
type HealthServer struct {
	server    *http.Server
	startTime time.Time
	version   string
	logger    *slog.Logger
	mu        sync.RWMutex
	scalers   map[string]RunnerCounter
	metrics   map[string]MetricsProvider
}

// ScaleSetStatus represents the status of a single scale set.
type ScaleSetStatus struct {
	Name           string          `json:"name"`
	Idle           int             `json:"idle"`
	Busy           int             `json:"busy"`
	Metrics        *MetricsStatus  `json:"metrics,omitempty"`
}

// MetricsStatus holds listener-level metrics for a scale set.
type MetricsStatus struct {
	JobsStarted    int64  `json:"jobs_started"`
	JobsCompleted  int64  `json:"jobs_completed"`
	DesiredRunners int    `json:"desired_runners"`
	AvailableJobs  int    `json:"available_jobs,omitempty"`
	AssignedJobs   int    `json:"assigned_jobs,omitempty"`
	RunningJobs    int    `json:"running_jobs,omitempty"`
}

// HealthResponse is the JSON response for /healthz.
type HealthResponse struct {
	Status    string           `json:"status"`
	Version   string           `json:"version"`
	Uptime    string           `json:"uptime"`
	ScaleSets []ScaleSetStatus `json:"scale_sets"`
}

// NewHealthServer creates a new health check HTTP server.
func NewHealthServer(port int, version string, logger *slog.Logger) *HealthServer {
	h := &HealthServer{
		startTime: time.Now(),
		version:   version,
		logger:    logger,
		scalers:   make(map[string]RunnerCounter),
		metrics:   make(map[string]MetricsProvider),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", h.handleHealthz)
	mux.HandleFunc("GET /readyz", h.handleReadyz)

	h.server = &http.Server{
		Handler: mux,
	}
	return h
}

// Serve starts the HTTP server on the given listener.
func (h *HealthServer) Serve(ln net.Listener) error {
	return h.server.Serve(ln)
}

// Shutdown gracefully shuts down the health server.
func (h *HealthServer) Shutdown(ctx context.Context) error {
	return h.server.Shutdown(ctx)
}

// RegisterScaler adds a scaler to be reported in health checks.
func (h *HealthServer) RegisterScaler(name string, s RunnerCounter) {
	h.mu.Lock()
	h.scalers[name] = s
	h.mu.Unlock()
}

// UnregisterScaler removes a scaler from health reporting.
func (h *HealthServer) UnregisterScaler(name string) {
	h.mu.Lock()
	delete(h.scalers, name)
	delete(h.metrics, name)
	h.mu.Unlock()
}

// RegisterMetrics associates a MetricsProvider with a named scale set.
func (h *HealthServer) RegisterMetrics(name string, m MetricsProvider) {
	h.mu.Lock()
	h.metrics[name] = m
	h.mu.Unlock()
}

func (h *HealthServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	resp := HealthResponse{
		Status:    "ok",
		Version:   h.version,
		Uptime:    time.Since(h.startTime).Truncate(time.Second).String(),
		ScaleSets: make([]ScaleSetStatus, 0, len(h.scalers)),
	}

	for name, s := range h.scalers {
		idle, busy := s.RunnerCounts()
		ss := ScaleSetStatus{
			Name: name,
			Idle: idle,
			Busy: busy,
		}
		if m, ok := h.metrics[name]; ok {
			snap := m.Snapshot()
			ms := &MetricsStatus{
				JobsStarted:    snap.JobsStarted,
				JobsCompleted:  snap.JobsCompleted,
				DesiredRunners: snap.DesiredRunners,
			}
			if snap.Statistics != nil {
				ms.AvailableJobs = snap.Statistics.TotalAvailableJobs
				ms.AssignedJobs = snap.Statistics.TotalAssignedJobs
				ms.RunningJobs = snap.Statistics.TotalRunningJobs
			}
			ss.Metrics = ms
		}
		resp.ScaleSets = append(resp.ScaleSets, ss)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.logger.Error("Failed to encode health response", slog.Any("error", err))
	}
}

func (h *HealthServer) handleReadyz(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	ready := len(h.scalers) > 0
	h.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
		if err := json.NewEncoder(w).Encode(map[string]string{"status": "not ready"}); err != nil {
			h.logger.Error("Failed to encode readyz response", slog.Any("error", err))
		}
		return
	}
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "ok"}); err != nil {
		h.logger.Error("Failed to encode readyz response", slog.Any("error", err))
	}
}
