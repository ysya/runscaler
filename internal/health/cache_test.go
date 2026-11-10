package health

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

type cacheTestCounter struct{}

func (cacheTestCounter) InstanceCounts() (int, int) { return 0, 0 }

func TestCacheHealthAffectsReadiness(t *testing.T) {
	h := NewHealthServer(0, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.RegisterController("set", cacheTestCounter{})
	h.MarkConnected("set")
	ready := httptest.NewRecorder()
	h.server.Handler.ServeHTTP(ready, httptest.NewRequest("GET", "/readyz", nil))
	if ready.Code != 200 {
		t.Fatalf("baseline readiness = %d", ready.Code)
	}
	h.SetCacheProvider(func() CacheStatus { return CacheStatus{Enabled: true, Ready: false, Entries: 2, Bytes: 8} })
	unready := httptest.NewRecorder()
	h.server.Handler.ServeHTTP(unready, httptest.NewRequest("GET", "/readyz", nil))
	if unready.Code != 503 {
		t.Fatalf("cache outage readiness = %d", unready.Code)
	}
	health := httptest.NewRecorder()
	h.server.Handler.ServeHTTP(health, httptest.NewRequest("GET", "/healthz", nil))
	if !strings.Contains(health.Body.String(), `"entries":2`) || !strings.Contains(health.Body.String(), `"ready":false`) {
		t.Fatalf("cache status absent: %s", health.Body.String())
	}
}
