package provider

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/ysya/runscaler/internal/config"
)

func TestDockerDiagnosticsUnknownMetadata(t *testing.T) {
	p := NewDockerProvider(config.ScaleSetConfig{}, &readinessDocker{running: true}, slog.Default())
	d, err := p.CollectDiagnostics(context.Background(), "id")
	if err != nil {
		t.Fatal(err)
	}
	if d.ExitCode != nil || d.OOMKilled != nil || d.FinishedAt != nil || d.RawLog != "" {
		t.Fatalf("unknown metadata fabricated: %+v", d)
	}
}

func TestDockerDiagnosticsExitMetadataAndLogBounds(t *testing.T) {
	docker := &readinessDocker{inspect: &container.InspectResponse{State: &container.State{Status: "exited", ExitCode: 137, OOMKilled: true, FinishedAt: "2026-11-07T00:00:00Z"}, Config: &container.Config{Tty: true}}, stream: io.NopCloser(strings.NewReader(strings.Repeat("x", 200*1024)))}
	p := NewDockerProvider(config.ScaleSetConfig{}, docker, slog.Default())
	p.EnableDiagnosticLogs(true)
	d, err := p.CollectDiagnostics(context.Background(), "id")
	if err != nil {
		t.Fatal(err)
	}
	if d.ExitCode == nil || *d.ExitCode != 137 || d.OOMKilled == nil || !*d.OOMKilled || d.FinishedAt == nil || len(d.RawLog) > 64*1024 {
		t.Fatalf("diagnostics=%+v", d)
	}
}

func TestDockerDiagnosticsFailureAndCancellation(t *testing.T) {
	p := NewDockerProvider(config.ScaleSetConfig{}, &readinessDocker{inspectErr: errors.New("removed")}, slog.Default())
	d, err := p.CollectDiagnostics(context.Background(), "id")
	if err == nil || d.ExitCode != nil || d.OOMKilled != nil {
		t.Fatalf("missing container=%+v err=%v", d, err)
	}
	r, w := io.Pipe()
	defer func() { _ = w.Close() }()
	called := make(chan struct{})
	p = NewDockerProvider(config.ScaleSetConfig{}, &readinessDocker{running: true, stream: r, logsCalled: called}, slog.Default())
	p.EnableDiagnosticLogs(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _, _ = p.CollectDiagnostics(ctx, "id"); close(done) }()
	<-called
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("diagnostic log read ignored cancellation")
	}
}
