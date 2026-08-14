package main

import (
	"strings"
	"testing"
	"time"

	"github.com/ysya/runscaler/internal/health"
)

func TestFormatStatusDetailedDashboard(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	h := health.HealthResponse{
		Status:  "degraded",
		Version: "0.6.0",
		Uptime:  "2h31m4s",
		ScaleSets: []health.ScaleSetStatus{
			{
				Name:          "linux-x64",
				Ready:         true,
				LastConnected: "2026-08-15T11:58:00Z",
				Idle:          2,
				Busy:          1,
				Metrics: &health.MetricsStatus{
					JobsStarted:    10,
					JobsCompleted:  8,
					DesiredRunners: 3,
					AvailableJobs:  4,
					AssignedJobs:   2,
					RunningJobs:    1,
				},
			},
			{
				Name:          "macos-arm64",
				LastError:     "listener disconnected\nretrying",
				LastConnected: "2026-08-15T10:00:00Z",
				Metrics: &health.MetricsStatus{
					JobsStarted:    3,
					JobsCompleted:  3,
					DesiredRunners: 2,
					AvailableJobs:  1,
					AssignedJobs:   1,
				},
			},
		},
		Disk: []health.DiskStatus{
			{Filesystem: "/Volumes/Tart", FreePercent: 25, FreeBytes: 25 << 30, TotalBytes: 100 << 30},
			{Filesystem: "/", FreePercent: 50, FreeBytes: 50 << 30, TotalBytes: 100 << 30},
		},
	}

	got := formatStatus(h, "http://127.0.0.1:9090/healthz", now, false)
	for _, want := range []string{
		"RUNNER STATUS",
		"● DEGRADED  runner 0.6.0 · uptime 2h31m4s",
		"Endpoint    http://127.0.0.1:9090/healthz",
		"Scale sets  1 ready / 2 total",
		"Runners     2 idle · 1 busy · 3 total · 5 desired",
		"Queue       5 available · 3 assigned · 1 running",
		"Jobs        13 started · 11 completed",
		"● READY  linux-x64",
		"2m ago · 2026-08-15T11:58:00Z",
		"● ERROR  macos-arm64",
		"2h ago · 2026-08-15T10:00:00Z",
		"listener disconnected\n                retrying",
		"DISK",
		"[████████░░░░░░░░]  50.0% free · 50.0 GiB available / 100.0 GiB total",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b[") {
		t.Fatalf("plain status output contains ANSI escapes:\n%q", got)
	}
	if strings.Index(got, "\n  /\n") > strings.Index(got, "\n  /Volumes/Tart\n") {
		t.Errorf("disk filesystems are not sorted:\n%s", got)
	}
}

func TestFormatStatusWithoutScaleSetsShowsWaiting(t *testing.T) {
	got := formatStatus(health.HealthResponse{
		Status:  "ok",
		Version: "0.6.0",
		Uptime:  "3s",
	}, "http://127.0.0.1:9090/healthz", time.Time{}, false)

	for _, want := range []string{
		"○ WAITING",
		"0 ready / 0 total",
		"0 idle · 0 busy · 0 total",
		"No scale sets registered yet. The runner may still be starting.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Queue") || strings.Contains(got, "Jobs") {
		t.Errorf("status output invented unavailable metrics:\n%s", got)
	}
}

func TestFormatStatusColorOptIn(t *testing.T) {
	got := formatStatus(health.HealthResponse{Status: "ok", ScaleSets: []health.ScaleSetStatus{{Name: "linux", Ready: true}}},
		"endpoint", time.Time{}, true)
	if !strings.Contains(got, "\x1b[") {
		t.Fatalf("colored status output contains no ANSI escapes: %q", got)
	}
}

func TestFormatStatusMarksPartialMetricsCoverage(t *testing.T) {
	h := health.HealthResponse{
		Status: "ok",
		ScaleSets: []health.ScaleSetStatus{
			{Name: "reported", Ready: true, Metrics: &health.MetricsStatus{DesiredRunners: 2}},
			{Name: "starting"},
		},
	}

	got := formatStatus(h, "endpoint", time.Time{}, false)
	if count := strings.Count(got, "1/2 scale sets reporting"); count != 3 {
		t.Errorf("partial metrics coverage count = %d, want 3:\n%s", count, got)
	}
	if !strings.Contains(got, "Metrics     not reported yet") {
		t.Errorf("missing per-scale-set metrics placeholder:\n%s", got)
	}
}

func TestFormatLastConnected(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "never", want: "not connected yet"},
		{name: "recent", value: "2026-08-15T11:59:30Z", want: "just now · 2026-08-15T11:59:30Z"},
		{name: "minutes", value: "2026-08-15T11:48:00Z", want: "12m ago · 2026-08-15T11:48:00Z"},
		{name: "days", value: "2026-08-13T11:00:00Z", want: "2d ago · 2026-08-13T11:00:00Z"},
		{name: "unknown format", value: "yesterday", want: "yesterday"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatLastConnected(tt.value, now); got != tt.want {
				t.Errorf("formatLastConnected() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDiskBarClampsPercentage(t *testing.T) {
	if got := diskBar(-10, 4); got != "[░░░░]" {
		t.Errorf("negative disk bar = %q", got)
	}
	if got := diskBar(50, 4); got != "[██░░]" {
		t.Errorf("half disk bar = %q", got)
	}
	if got := diskBar(110, 4); got != "[████]" {
		t.Errorf("overflow disk bar = %q", got)
	}
}
