package actionscache

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ysya/runscaler/internal/config"
)

type closedListener struct{ done chan struct{} }

func (l *closedListener) Accept() (net.Conn, error) { <-l.done; return nil, net.ErrClosed }
func (l *closedListener) Close() error {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}
func (l *closedListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("172.18.0.1"), Port: 32500}
}

type failingListener struct{}

func (failingListener) Accept() (net.Conn, error) {
	return nil, errors.New("simulated listener failure")
}
func (failingListener) Close() error { return nil }
func (failingListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("172.18.0.1"), Port: 32500}
}

func TestServiceReportsListenerFailureForManagerCancellation(t *testing.T) {
	cfg := config.ActionsCacheConfig{Enabled: true, ListenAddress: "172.18.0.1:32500", AdvertiseURL: "http://cache.local:32500/", DataDir: filepath.Join(t.TempDir(), "native"), MaxSizeBytes: 1024, TokenIssuer: "https://issuer.example", JWKSURL: "https://issuer.example/keys"}
	service, err := NewService(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	service.verifier.client = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(jwksResponse(key, "key1"))), Header: make(http.Header)}, nil
	})}
	service.listen = func(string, string) (net.Listener, error) { return failingListener{}, nil }
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-service.Failures():
		if err == nil {
			t.Fatal("reported nil listener failure")
		}
	case <-time.After(time.Second):
		t.Fatal("listener failure did not reach manager")
	}
	if service.Status().Ready {
		t.Fatal("receiver remained ready after listener failure")
	}
}

func TestServiceStartBootstrapsBeforeServingAndShutsDown(t *testing.T) {
	cfg := config.ActionsCacheConfig{Enabled: true, ListenAddress: "172.18.0.1:32500", AdvertiseURL: "http://cache.local:32500/", DataDir: filepath.Join(t.TempDir(), "native"), MaxSizeBytes: 1024, TokenIssuer: "https://issuer.example", JWKSURL: "https://issuer.example/keys"}
	service, err := NewService(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	listener := &closedListener{done: make(chan struct{})}
	listenCalls := 0
	service.listen = func(network, address string) (net.Listener, error) { listenCalls++; return listener, nil }
	service.verifier.client = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("outage")), Header: make(http.Header)}, nil
	})}
	if err := service.Start(context.Background()); err == nil {
		t.Fatal("started without JWKS")
	}
	if listenCalls != 0 || service.Status().Ready {
		t.Fatal("served before trusted key bootstrap")
	}
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	service.verifier.client = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(jwksResponse(key, "key1"))), Header: make(http.Header)}, nil
	})}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if listenCalls != 1 || !service.Status().Ready {
		t.Fatalf("listener state: calls=%d status=%+v", listenCalls, service.Status())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := service.Shutdown(ctx); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	if service.Status().Ready {
		t.Fatal("ready after shutdown")
	}
}
