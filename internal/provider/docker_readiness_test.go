package provider

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	dockerclient "github.com/moby/moby/client"

	"github.com/ysya/runscaler/internal/config"
)

type readinessDocker struct {
	DockerAPI
	stream     io.ReadCloser
	logErr     error
	running    bool
	logsCalled chan struct{}
	inspect    *container.InspectResponse
	inspectErr error
}

func (d *readinessDocker) ContainerLogs(context.Context, string, dockerclient.ContainerLogsOptions) (dockerclient.ContainerLogsResult, error) {
	if d.logsCalled != nil {
		close(d.logsCalled)
	}
	return d.stream, d.logErr
}
func (d *readinessDocker) ContainerInspect(context.Context, string, dockerclient.ContainerInspectOptions) (dockerclient.ContainerInspectResult, error) {
	if d.inspect != nil {
		return dockerclient.ContainerInspectResult{Container: *d.inspect}, d.inspectErr
	}
	if d.inspectErr != nil {
		return dockerclient.ContainerInspectResult{}, d.inspectErr
	}
	return dockerclient.ContainerInspectResult{Container: container.InspectResponse{State: &container.State{Running: d.running}, Config: &container.Config{}}}, nil
}

func TestDockerReadinessMultiplexedSplitMarkerAndSettle(t *testing.T) {
	var framed bytes.Buffer
	for _, part := range []string{strings.Repeat("x", 16000), "Listen", "ing for Jobs"} {
		var header [8]byte
		header[0] = byte(stdcopy.Stdout)
		binary.BigEndian.PutUint32(header[4:], uint32(len(part)))
		framed.Write(header[:])
		framed.WriteString(part)
	}
	p := NewDockerProvider(config.ScaleSetConfig{}, &readinessDocker{stream: io.NopCloser(&framed), running: true}, slog.Default())
	p.readySettle = 20 * time.Millisecond
	p.readyPoll = time.Millisecond
	p.readyTimeout = time.Second
	started := time.Now()
	if err := p.WaitReady(context.Background(), "id"); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) < p.readySettle {
		t.Fatal("runner accepted before settle interval")
	}
}

func TestDockerReadinessUnavailableAndEarlyExit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running bool
		want    error
	}{{"unreadable", true, ErrReadinessUnavailable}, {"exited", false, ErrExitedBeforeReady}} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewDockerProvider(config.ScaleSetConfig{}, &readinessDocker{running: tc.running, logErr: errors.New("private daemon error")}, slog.Default())
			p.readyTimeout = 20 * time.Millisecond
			p.readyPoll = time.Millisecond
			if err := p.WaitReady(context.Background(), "id"); !errors.Is(err, tc.want) || strings.Contains(err.Error(), "private") {
				t.Fatalf("readiness error=%v", err)
			}
		})
	}
}

func TestDockerReadinessCancellationClosesStream(t *testing.T) {
	r, w := io.Pipe()
	defer func() { _ = w.Close() }()
	called := make(chan struct{})
	p := NewDockerProvider(config.ScaleSetConfig{}, &readinessDocker{stream: r, running: true, logsCalled: called}, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.WaitReady(ctx, "id") }()
	<-called
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	if _, err := w.Write([]byte("after cancellation")); err == nil {
		t.Fatal("cancelled log stream remained open")
	}
}

func TestDockerReadinessRejectsUnboundedFrame(t *testing.T) {
	var header [8]byte
	header[0] = 1
	binary.BigEndian.PutUint32(header[4:], ^uint32(0))
	p := NewDockerProvider(config.ScaleSetConfig{}, &readinessDocker{stream: io.NopCloser(bytes.NewReader(header[:])), running: true}, slog.Default())
	p.readyTimeout = 20 * time.Millisecond
	p.readyPoll = time.Millisecond
	if err := p.WaitReady(context.Background(), "id"); !errors.Is(err, ErrReadinessUnavailable) {
		t.Fatalf("oversized frame=%v", err)
	}
}
