package provider

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	dockerclient "github.com/moby/moby/client"
)

const readyMarker = "Listening for Jobs"

// defaultReadySettle is how long a runner must stay alive after printing
// readyMarker. A runner whose version GitHub has retired connects, prints
// the marker, then is refused on its first poll for messages and exits.
const defaultReadySettle = 5 * time.Second
const maxDockerLogFrame = 1024 * 1024

var _ InstanceReadiness = (*DockerProvider)(nil)

func (p *DockerProvider) WaitReady(parent context.Context, id string) error {
	if !p.readiness {
		return ErrReadinessDisabled
	}
	ctx, cancel := context.WithTimeout(parent, p.readyTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	initial, err := p.dockerClient.ContainerInspect(ctx, id, dockerclient.ContainerInspectOptions{})
	if err == nil && initial.Container.State != nil && !initial.Container.State.Running {
		return ErrExitedBeforeReady
	}
	unavailable := err != nil || initial.Container.State == nil || initial.Container.Config == nil
	marker := make(chan struct{})
	var finished <-chan struct{}
	if !unavailable {
		stream, logErr := p.dockerClient.ContainerLogs(ctx, id, dockerclient.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Follow: true, Tail: "200"})
		if logErr != nil {
			unavailable = true
		} else {
			done := make(chan struct{})
			finished = done
			stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
			defer func() { stop(); _ = stream.Close(); <-done }()
			go func() {
				defer close(done)
				var once sync.Once
				found := func() { once.Do(func() { close(marker) }) }
				out, stderr := &readyMarkerWriter{found: found}, &readyMarkerWriter{found: found}
				if initial.Container.Config.Tty {
					_, _ = io.Copy(out, stream)
				} else {
					_, _ = stdcopy.StdCopy(out, stderr, &boundedDockerFrames{source: stream})
				}
			}()
		}
	}
	ticker := time.NewTicker(p.readyPoll)
	defer ticker.Stop()
	var marked time.Time
	markerC := (<-chan struct{})(marker)
	for {
		select {
		case <-ctx.Done():
			if parent.Err() != nil {
				return parent.Err()
			}
			if unavailable {
				return ErrReadinessUnavailable
			}
			return ErrReadinessTimeout
		case <-markerC:
			marked = time.Now()
			markerC = nil
		case <-finished:
			unavailable = true
			finished = nil
		case <-ticker.C:
			result, inspectErr := p.dockerClient.ContainerInspect(ctx, id, dockerclient.ContainerInspectOptions{})
			if inspectErr != nil || result.Container.State == nil {
				unavailable = true
				continue
			}
			if !result.Container.State.Running {
				return ErrExitedBeforeReady
			}
			if !marked.IsZero() && time.Since(marked) >= p.readySettle {
				return nil
			}
		}
	}
}

// Keep a marker-sized suffix rather than retaining runner output in memory.
type readyMarkerWriter struct {
	tail  string
	found func()
}

func (w *readyMarkerWriter) Write(p []byte) (int, error) {
	s := w.tail + string(p)
	if strings.Contains(s, readyMarker) {
		w.found()
	}
	if len(s) >= len(readyMarker) {
		s = s[len(s)-len(readyMarker)+1:]
	}
	w.tail = s
	return len(p), nil
}

// StdCopy trusts frame lengths. Reject oversized/invalid headers before they
// reach it, so a damaged daemon stream cannot request unbounded allocations.
type boundedDockerFrames struct {
	source    io.Reader
	header    [8]byte
	headerPos int
	remaining uint32
	inFrame   bool
}

func (r *boundedDockerFrames) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if !r.inFrame {
		if _, err := io.ReadFull(r.source, r.header[:]); err != nil {
			return 0, err
		}
		r.remaining = binary.BigEndian.Uint32(r.header[4:])
		if r.remaining > maxDockerLogFrame || r.header[1] != 0 || r.header[2] != 0 || r.header[3] != 0 {
			return 0, errors.New("invalid Docker log frame")
		}
		r.headerPos = 0
		r.inFrame = true
	}
	if r.headerPos < 8 {
		n := copy(p, r.header[r.headerPos:])
		r.headerPos += n
		if r.headerPos == 8 && r.remaining == 0 {
			r.inFrame = false
		}
		return n, nil
	}
	if uint32(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.source.Read(p)
	r.remaining -= uint32(n)
	if r.remaining == 0 {
		r.inFrame = false
	}
	return n, err
}
