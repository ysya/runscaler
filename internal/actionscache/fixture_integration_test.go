package actionscache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestNativeProtocolFixture is opt-in because it starts the repository's Node
// protocol fixture and binds two loopback listeners. It never starts a runner.
func TestNativeProtocolFixture(t *testing.T) {
	if os.Getenv("RUNSCALER_NATIVE_FIXTURE") != "1" {
		t.Skip("set RUNSCALER_NATIVE_FIXTURE=1 to run the local protocol fixture")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal(err)
	}

	issuerListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	issuerURL := "http://" + issuerListener.Addr().String()
	if err := issuerListener.Close(); err != nil {
		t.Fatal(err)
	}
	parsedIssuer, err := url.Parse(issuerURL)
	if err != nil {
		t.Fatal(err)
	}
	cacheListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cacheURL := "http://" + cacheListener.Addr().String()

	store, err := OpenStore(filepath.Join(t.TempDir(), "native"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	verifier, err := NewVerifier(VerifierConfig{
		Issuer: issuerURL, JWKSURL: issuerURL + "/jwks", Audiences: []string{"runscaler-gate"},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(HandlerConfig{
		Store: store, Verifier: verifier, BaseURL: cacheURL + "/",
		UpstreamURL: "https://results.example", UpstreamTransport: transportFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != "results.example" {
				return nil, fmt.Errorf("unexpected upstream %q", r.URL.Host)
			}
			forward := r.Clone(r.Context())
			forward.URL.Scheme = parsedIssuer.Scheme
			forward.URL.Host = parsedIssuer.Host
			forward.Host = parsedIssuer.Host
			return http.DefaultTransport.RoundTrip(forward)
		}),
		MaxSizeBytes: 32 << 20, MaxIdle: 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	cacheServer := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = cacheServer.Serve(cacheListener) }()
	t.Cleanup(func() { _ = cacheServer.Close() })

	logFile, err := os.CreateTemp(t.TempDir(), "fixture-log-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "../../scripts/local-cache/cache-protocol-fixture.mjs")
	cmd.Env = append(os.Environ(),
		"PROBE_CACHE_URL="+cacheURL+"/",
		"PROBE_ISSUER_URL="+issuerURL,
		"PROBE_BIND_HOST=127.0.0.1",
		"PROBE_UNKNOWN_DOWNLOAD_STATUS=403",
	)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		_ = cmd.Wait()
	}()
	client := &http.Client{Timeout: 12 * time.Second}
	ready := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		response, err := client.Get(issuerURL + "/jwks")
		if err == nil {
			_ = response.Body.Close()
			ready = response.StatusCode == http.StatusOK
			if ready {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		content, _ := os.ReadFile(logFile.Name())
		t.Fatalf("fixture did not start: %s", content)
	}
	for _, phase := range []string{"roundtrip", "restore", "cancellation", "cancellation-restore"} {
		input, _ := json.Marshal(map[string]string{"phase": phase})
		response, err := client.Post(issuerURL+"/run", "application/json", bytes.NewReader(input))
		if err != nil {
			t.Fatalf("fixture %s: %v", phase, err)
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("fixture %s: status %d, read error %v, body %s", phase, response.StatusCode, err, body)
		}
		var result struct {
			Phase   string   `json:"phase"`
			Results []string `json:"results"`
		}
		if err := json.Unmarshal(body, &result); err != nil || result.Phase != phase || len(result.Results) == 0 {
			t.Fatalf("fixture %s: invalid result %s (%v)", phase, body, err)
		}
		t.Logf("fixture %s: %v", phase, result.Results)
	}
}
