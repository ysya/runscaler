package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/pkg/authconfig"
	"github.com/moby/moby/api/types/registry"
	dockerclient "github.com/moby/moby/client"

	"github.com/ysya/runscaler/internal/config"
)

func TestPrepareRunnerImagesIncludesPrivateDaemons(t *testing.T) {
	a := validScaleSet("a", 0)
	a.RunnerImage = "runner:test"
	a.Docker.DaemonMode = "dind"
	b := a
	b.Docker.Platform = "linux/arm64"
	b.Docker.DinDImage = "custom:dind"
	var got []string
	err := prepareRunnerImages(context.Background(), []config.ScaleSetConfig{a, a, b}, func(_ context.Context, ss config.ScaleSetConfig) error {
		got = append(got, ss.RunnerImage+"|"+ss.Docker.Platform)
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"runner:test|", "docker:29-dind|", "runner:test|linux/arm64", "custom:dind|linux/arm64"}) {
		t.Fatalf("missing or duplicated required images: %v", got)
	}
}

func TestPreflightUsesDockerLoginForEachImage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"auths":{"ghcr.io":{"auth":"dXNlcjpwYXNz"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get(registry.AuthHeader)
		switch image := r.URL.Query().Get("fromImage"); image {
		case "ghcr.io/example/runner":
			auth, err := authconfig.Decode(header)
			if err != nil || auth.Username != "user" || auth.Password != "pass" || auth.ServerAddress != "ghcr.io" {
				t.Error("private image did not receive its Docker login")
			}
		case "docker.io/library/docker":
			if header != "" {
				t.Error("public DinD image received registry auth")
			}
		default:
			t.Errorf("unexpected Docker request: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{}\n")
	}))
	defer server.Close()
	client, err := dockerclient.New(dockerclient.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")), dockerclient.WithAPIVersion("1.49"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	ss := validScaleSet("private", 0)
	ss.RunnerImage = "ghcr.io/example/runner:latest"
	ss.Docker.DaemonMode = "dind"
	clients := map[string]*dockerclient.Client{ss.Docker.Socket: client}
	if err := prepareRunnerImages(t.Context(), []config.ScaleSetConfig{ss}, func(ctx context.Context, target config.ScaleSetConfig) error {
		return pullDockerRunnerImage(ctx, target, clients, io.Discard, slog.New(slog.DiscardHandler))
	}, nil); err != nil {
		t.Fatal(err)
	}
}
