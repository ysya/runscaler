package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"sort"
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
	states    map[string]connectionState
	diskFn    func() []DiskStatus
}

type connectionState struct {
	Ready         bool
	LastError     string
	LastConnected time.Time
}

// ScaleSetStatus represents the status of a single scale set.
type ScaleSetStatus struct {
	Name          string         `json:"name"`
	Ready         bool           `json:"ready"`
	LastError     string         `json:"last_error,omitempty"`
	LastConnected string         `json:"last_connected,omitempty"`
	Idle          int            `json:"idle"`
	Busy          int            `json:"busy"`
	Metrics       *MetricsStatus `json:"metrics,omitempty"`
}

// MetricsStatus holds listener-level metrics for a scale set.
type MetricsStatus struct {
	JobsStarted    int64 `json:"jobs_started"`
	JobsCompleted  int64 `json:"jobs_completed"`
	DesiredRunners int   `json:"desired_runners"`
	AvailableJobs  int   `json:"available_jobs,omitempty"`
	AssignedJobs   int   `json:"assigned_jobs,omitempty"`
	RunningJobs    int   `json:"running_jobs,omitempty"`
}

// DiskStatus reports capacity for one filesystem a configured
// cachestore.CacheStore lives on. It is populated from
// internal/diskguard.StatFor only — a plain statfs — never from a store's
// Measure, which can be expensive (it may walk a volume or spawn a helper
// container): /healthz can be polled, so its disk section must stay cheap
// on every call. For each store's actual measured usage, see the
// 'runner cache' command instead.
type DiskStatus struct {
	Filesystem  string  `json:"filesystem"`
	FreePercent float64 `json:"free_percent"`
	FreeBytes   uint64  `json:"free_bytes"`
	TotalBytes  uint64  `json:"total_bytes"`
}

// HealthResponse is the JSON response for /healthz.
type HealthResponse struct {
	Status    string           `json:"status"`
	Version   string           `json:"version"`
	Uptime    string           `json:"uptime"`
	ScaleSets []ScaleSetStatus `json:"scale_sets"`
	Disk      []DiskStatus     `json:"disk,omitempty"`
}

// NewHealthServer creates a new health check HTTP server.
func NewHealthServer(port int, version string, logger *slog.Logger) *HealthServer {
	h := &HealthServer{
		startTime: time.Now(),
		version:   version,
		logger:    logger,
		scalers:   make(map[string]RunnerCounter),
		metrics:   make(map[string]MetricsProvider),
		states:    make(map[string]connectionState),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", h.handleHealthz)
	mux.HandleFunc("GET /readyz", h.handleReadyz)

	h.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
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
	h.states[name] = connectionState{}
	h.mu.Unlock()
}

// UnregisterScaler removes a scaler from health reporting.
func (h *HealthServer) UnregisterScaler(name string) {
	h.mu.Lock()
	delete(h.scalers, name)
	delete(h.metrics, name)
	delete(h.states, name)
	h.mu.Unlock()
}

func (h *HealthServer) MarkConnected(name string) {
	h.mu.Lock()
	state := h.states[name]
	state.Ready = true
	state.LastError = ""
	state.LastConnected = time.Now()
	h.states[name] = state
	h.mu.Unlock()
}

func (h *HealthServer) MarkDisconnected(name, reason string) {
	h.mu.Lock()
	state := h.states[name]
	state.Ready = false
	state.LastError = reason
	h.states[name] = state
	h.mu.Unlock()
}

// RegisterMetrics associates a MetricsProvider with a named scale set.
func (h *HealthServer) RegisterMetrics(name string, m MetricsProvider) {
	h.mu.Lock()
	h.metrics[name] = m
	h.mu.Unlock()
}

// SetDiskProvider registers the function /healthz calls to populate its
// disk section — see DiskStatus's doc comment for why it must stay cheap.
// main wires this to a function that calls internal/diskguard.StatFor per
// configured cachestore.CacheStore, never Measure. Until this is called,
// /healthz reports no disk section at all (Disk stays nil, omitted by its
// omitempty tag) — the same "empty until registered" shape RegisterScaler
// gives ScaleSets before any scale set has started.
func (h *HealthServer) SetDiskProvider(fn func() []DiskStatus) {
	h.mu.Lock()
	h.diskFn = fn
	h.mu.Unlock()
}

func (h *HealthServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	status := "ok"
	for name := range h.scalers {
		if !h.states[name].Ready {
			status = "degraded"
			break
		}
	}
	resp := HealthResponse{
		Status:    status,
		Version:   h.version,
		Uptime:    time.Since(h.startTime).Truncate(time.Second).String(),
		ScaleSets: make([]ScaleSetStatus, 0, len(h.scalers)),
	}
	if h.diskFn != nil {
		resp.Disk = h.diskFn()
	}

	names := make([]string, 0, len(h.scalers))
	for name := range h.scalers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		s := h.scalers[name]
		idle, busy := s.RunnerCounts()
		ss := ScaleSetStatus{
			Name: name,
			Idle: idle,
			Busy: busy,
		}
		state := h.states[name]
		ss.Ready = state.Ready
		ss.LastError = state.LastError
		if !state.LastConnected.IsZero() {
			ss.LastConnected = state.LastConnected.Format(time.RFC3339)
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
	for name := range h.scalers {
		if !h.states[name].Ready {
			ready = false
			break
		}
	}
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
