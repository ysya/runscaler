package main

import (
	"bytes"
	"github.com/ysya/runscaler/internal/config"
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
