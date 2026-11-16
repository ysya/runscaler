package main

import (
	"bytes"
	"encoding/json"
	"github.com/ysya/runscaler/internal/config"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTartSoftnetDependencyChecks(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	var out bytes.Buffer
	sets := []config.ScaleSetConfig{{Provider: "tart"}}
	if err := checkTartSoftnet(sets, &out); err != nil {
		t.Fatal(err)
	}
	sets[0].Tart.NetSoftnet = true
	if err := checkTartSoftnet(sets, &out); err == nil {
		t.Fatal("missing Softnet accepted")
	}
	if !strings.Contains(out.String(), "✗") {
		t.Fatal("missing dependency was not reported as a failed check")
	}
	if err := os.WriteFile(filepath.Join(dir, "softnet"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := checkTartSoftnet(sets, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "passwordless sudo") {
		t.Fatalf("privilege guidance missing: %s", &out)
	}
}

func TestSoftnetRunDiagnosticsUseStructuredLogger(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	if err := os.WriteFile(filepath.Join(dir, "softnet"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, nil))
	if err := checkTartSoftnetForRun([]config.ScaleSetConfig{{Provider: "tart", Tart: config.TartConfig{NetSoftnet: true}}}, logger); err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(out.Bytes(), &record); err != nil {
		t.Fatalf("diagnostic corrupted JSON logging: %v; %q", err, out.String())
	}
	if record["level"] != "WARN" || !strings.Contains(record["msg"].(string), "passwordless sudo") {
		t.Fatalf("missing privilege guidance: %v", record)
	}
}
