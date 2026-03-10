package health

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"time"
)

// RunnerCounter provides runner count information.
type RunnerCounter interface {
	RunnerCounts() (idle, busy int)
}

// HealthServer provides /healthz and /readyz endpoints for monitoring.
type HealthServer struct {
	server    *http.Server
	startTime time.Time
	version   string
	mu        sync.RWMutex
	scalers   map[string]RunnerCounter
}

// ScaleSetStatus represents the status of a single scale set.
type ScaleSetStatus struct {
	Name string `json:"name"`
	Idle int    `json:"idle"`
	Busy int    `json:"busy"`
}

// HealthResponse is the JSON response for /healthz.
type HealthResponse struct {
	Status    string           `json:"status"`
	Version   string           `json:"version"`
	Uptime    string           `json:"uptime"`
	ScaleSets []ScaleSetStatus `json:"scale_sets"`
}

// NewHealthServer creates a new health check HTTP server.
func NewHealthServer(port int, version string) *HealthServer {
	h := &HealthServer{
		startTime: time.Now(),
		version:   version,
		scalers:   make(map[string]RunnerCounter),
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
		resp.ScaleSets = append(resp.ScaleSets, ScaleSetStatus{
			Name: name,
			Idle: idle,
			Busy: busy,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *HealthServer) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
