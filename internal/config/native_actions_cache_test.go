package config

import (
	"strings"
	"testing"
)

func TestNativeActionsCacheConfigIsGlobalAndOptIn(t *testing.T) {
	disabled := loadTOML(t, `log-level = "info"
log-format = "text"
url = "https://github.com/org"
name = "runners"
token = "fixture"
`)
	if disabled.ActionsCache.Enabled {
		t.Fatal("native cache enabled by default")
	}
	if err := disabled.ValidateGlobal(); err != nil {
		t.Fatalf("disabled config invalid: %v", err)
	}
	cfg := loadTOML(t, `
log-level = "info"
log-format = "text"
[actions-cache]
enabled = true
listen-address = "172.18.0.1:32500"
advertise-url = "http://cache.internal:32500/"
data-dir = "/var/lib/runner/native-actions-cache"
max-size-bytes = 53687091200

[[scaleset]]
url = "https://github.com/org"
name = "runners"
token = "fixture"
`)
	if len(cfg.Warnings) != 0 {
		t.Fatalf("unexpected config warnings: %q", cfg.Warnings)
	}
	if !cfg.ActionsCache.Enabled || cfg.ActionsCache.MaxSizeBytes != 53687091200 {
		t.Fatalf("native cache settings lost: %+v", cfg.ActionsCache)
	}
	if err := cfg.ValidateGlobal(); err != nil {
		t.Fatalf("valid native cache config: %v", err)
	}
}

func TestNativeActionsCacheRejectsUnsafeListenerAndURLs(t *testing.T) {
	base := ActionsCacheConfig{Enabled: true, ListenAddress: "172.18.0.1:32500", AdvertiseURL: "http://cache.internal:32500/", DataDir: "/var/lib/runner/native-actions-cache", MaxSizeBytes: 1024}
	for _, test := range []struct {
		name   string
		change func(*ActionsCacheConfig)
		want   string
	}{
		{"wildcard listener", func(c *ActionsCacheConfig) { c.ListenAddress = "0.0.0.0:32500" }, "private"},
		{"public listener", func(c *ActionsCacheConfig) { c.ListenAddress = "8.8.8.8:32500" }, "private"},
		{"missing port", func(c *ActionsCacheConfig) { c.ListenAddress = "172.18.0.1" }, "listen-address"},
		{"credentials", func(c *ActionsCacheConfig) { c.AdvertiseURL = "http://user:pass@cache.internal:32500/" }, "advertise-url"},
		{"missing trailing slash", func(c *ActionsCacheConfig) { c.AdvertiseURL = "http://cache.internal:32500" }, "advertise-url"},
		{"advertise subpath", func(c *ActionsCacheConfig) { c.AdvertiseURL = "http://cache.internal:32500/cache/" }, "advertise-url"},
		{"relative data dir", func(c *ActionsCacheConfig) { c.DataDir = "relative" }, "data-dir"},
		{"zero budget", func(c *ActionsCacheConfig) { c.MaxSizeBytes = 0 }, "max-size-bytes"},
		{"untrusted upstream", func(c *ActionsCacheConfig) { c.UpstreamResultsURL = "http://results.example" }, "upstream-results-url"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			test.change(&cfg)
			if err := cfg.Validate(9090); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate = %v, want %q", err, test.want)
			}
		})
	}
	if err := base.Validate(32500); err == nil || !strings.Contains(err.Error(), "health-port") {
		t.Fatalf("port collision: %v", err)
	}
}
