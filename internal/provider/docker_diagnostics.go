package provider

import (
	"bytes"
	"context"
	"io"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	dockerclient "github.com/moby/moby/client"
)

var _ InstanceDiagnostics = (*DockerProvider)(nil)

// EnableDiagnosticLogs opts into collecting a bounded raw excerpt. Call before
// starting workers. Redaction and private persistence remain the caller's job.
func (p *DockerProvider) EnableDiagnosticLogs(enabled bool) { p.captureLogs = enabled }

func (p *DockerProvider) CollectDiagnostics(ctx context.Context, id string) (ExitDiagnostics, error) {
	var d ExitDiagnostics
	inspector, ok := p.dockerClient.(dockerInspector)
	if !ok {
		return d, ErrReadinessUnavailable
	}
	result, err := inspector.ContainerInspect(ctx, id, dockerclient.ContainerInspectOptions{})
	if err != nil {
		return d, err
	}
	i := result.Container
	if i.State != nil {
		finished, e := time.Parse(time.RFC3339Nano, i.State.FinishedAt)
		if e == nil && !finished.IsZero() {
			d.FinishedAt = &finished
		}
		if string(i.State.Status) == "exited" || string(i.State.Status) == "dead" || d.FinishedAt != nil {
			exit, oom := i.State.ExitCode, i.State.OOMKilled
			d.ExitCode = &exit
			d.OOMKilled = &oom
		}
	}
	if !p.captureLogs {
		return d, nil
	}
	stream, err := p.dockerClient.ContainerLogs(ctx, id, dockerclient.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Tail: "100"})
	if err != nil {
		return d, err
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stop()
	var out bytes.Buffer
	writer := &limitedLogWriter{out: &out, remaining: 64 * 1024}
	// Bound input as well as output (including framing overhead).
	input := io.LimitReader(stream, 128*1024)
	if i.Config != nil && i.Config.Tty {
		_, err = io.Copy(writer, input)
	} else {
		_, err = stdcopy.StdCopy(writer, writer, &boundedDockerFrames{source: input})
	}
	d.RawLog = out.String()
	return d, err
}

type limitedLogWriter struct {
	out       *bytes.Buffer
	remaining int
}

func (w *limitedLogWriter) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > w.remaining {
		p = p[:w.remaining]
	}
	_, _ = w.out.Write(p)
	w.remaining -= len(p)
	return n, nil
}
