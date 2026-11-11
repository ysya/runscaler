package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const runnerWorkPath = "/home/runner/_work"
const maxDockerCreateBody = 16 << 20

var dockerCreatePath = regexp.MustCompile(`^(/v[0-9.]+)?/containers/create$`)

// workspaceProxy keeps the runner's stable _work path while translating its
// bind sources to the private host directory visible to the Docker daemon.
// The socket lives only on the host and is mounted into one runner.
type workspaceProxy struct {
	server    *http.Server
	listener  net.Listener
	transport *http.Transport
}

func startWorkspaceProxy(socketPath, daemonSocket, hostWork string) (*workspaceProxy, error) {
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen on runner Docker socket: %w", err)
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		return nil, errors.Join(fmt.Errorf("restrict runner Docker socket: %w", err), listener.Close())
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", daemonSocket)
	}}
	upstream, _ := url.Parse("http://docker")
	reverse := httputil.NewSingleHostReverseProxy(upstream)
	reverse.Transport = transport
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && dockerCreatePath.MatchString(r.URL.Path) {
			if err := rewriteDockerCreate(r, hostWork); err != nil {
				http.Error(w, "invalid Docker container create request", http.StatusBadRequest)
				return
			}
		}
		reverse.ServeHTTP(w, r)
	})}
	p := &workspaceProxy{server: server, listener: listener, transport: transport}
	go func() { _ = server.Serve(listener) }()
	return p, nil
}

func (p *workspaceProxy) Close() error {
	err := p.server.Close()
	p.transport.CloseIdleConnections()
	return err
}

func rewriteDockerCreate(r *http.Request, hostWork string) error {
	if r.ContentLength > maxDockerCreateBody {
		return errors.New("docker create body too large")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxDockerCreateBody+1))
	if err != nil {
		return err
	}
	if len(body) > maxDockerCreateBody {
		return errors.New("docker create body too large")
	}
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		return err
	}
	var host map[string]json.RawMessage
	if err := json.Unmarshal(request["HostConfig"], &host); err != nil {
		return err
	}
	if len(host) == 0 {
		return errors.New("missing Docker HostConfig")
	}
	if raw, ok := host["Binds"]; ok && string(raw) != "null" {
		var binds []string
		if err := json.Unmarshal(raw, &binds); err != nil {
			return err
		}
		for i, bind := range binds {
			source, rest, found := strings.Cut(bind, ":")
			if !found {
				continue
			}
			if translated, ok := translateWorkspaceSource(source, hostWork); ok {
				binds[i] = translated + ":" + rest
			}
		}
		host["Binds"], _ = json.Marshal(binds)
	}
	if raw, ok := host["Mounts"]; ok && string(raw) != "null" {
		var mounts []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &mounts); err != nil {
			return err
		}
		for _, m := range mounts {
			var kind, source string
			if err := json.Unmarshal(m["Type"], &kind); err != nil {
				return err
			}
			if kind != "bind" {
				continue
			}
			if err := json.Unmarshal(m["Source"], &source); err != nil {
				return err
			}
			if translated, ok := translateWorkspaceSource(source, hostWork); ok {
				m["Source"], _ = json.Marshal(translated)
			}
		}
		host["Mounts"], _ = json.Marshal(mounts)
	}
	request["HostConfig"], _ = json.Marshal(host)
	encoded, err := json.Marshal(request)
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(encoded))
	r.ContentLength = int64(len(encoded))
	r.Header.Del("Transfer-Encoding")
	r.TransferEncoding = nil
	return nil
}

func translateWorkspaceSource(source, hostWork string) (string, bool) {
	clean := filepath.Clean(source)
	if clean == runnerWorkPath {
		return hostWork, true
	}
	if !strings.HasPrefix(clean, runnerWorkPath+"/") {
		return "", false
	}
	return filepath.Join(hostWork, strings.TrimPrefix(clean, runnerWorkPath+"/")), true
}
