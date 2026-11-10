package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func renderLocalCacheDeployment(t *testing.T, missing string) ([]byte, error) {
	t.Helper()
	compose, err := filepath.Abs("../../deploy/local-cache/compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(compose); err != nil {
		t.Fatalf("local cache service bundle missing: %v", err)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker Compose CLI is not installed")
	}
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("Docker Compose CLI is not installed")
	}
	values := map[string]string{
		"CACHE_BIND_IP": "192.168.50.10", "CACHE_PORT": "13000",
		"CACHE_ADVERTISE_URL":  "http://192.168.50.10:13000/",
		"CACHE_MAX_SIZE_BYTES": "1073741824", "CACHE_MANAGEMENT_KEY": "controlled-test-only-key",
	}
	values[missing] = ""
	dir := t.TempDir()
	var env strings.Builder
	for key, value := range values {
		if key != "" {
			env.WriteString(key + "=" + value + "\n")
		}
	}
	envFile := filepath.Join(dir, "fixture.env")
	if err := os.WriteFile(envFile, []byte(env.String()), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("docker", "compose", "--env-file", envFile, "-f", compose, "config", "--format", "json")
	command.Dir = dir
	// Preserve CLI plugin discovery (some installations locate Compose through
	// Docker's user config), while refusing ambient deployment inputs.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "CACHE_") || strings.HasPrefix(key, "COMPOSE_") || key == "DOCKER_HOST" || key == "DOCKER_CONTEXT" {
			continue
		}
		command.Env = append(command.Env, entry)
	}
	command.Env = append(command.Env, "COMPOSE_PROJECT_NAME=runscaler-cache-fixture")
	return command.CombinedOutput()
}

func TestLocalCacheDeploymentContract(t *testing.T) {
	out, err := renderLocalCacheDeployment(t, "")
	if err != nil {
		t.Fatalf("compose render: %v: %s", err, out)
	}
	var model struct {
		Services map[string]struct {
			Image         string
			Platform      string
			ContainerName string `json:"container_name"`
			Restart       string
			Environment   map[string]string
			Labels        map[string]string
			Volumes       []struct{ Type, Source, Target string }
			Ports         []struct {
				HostIP    string `json:"host_ip"`
				Published string
				Target    int
			}
			Healthcheck map[string]any
		}
		Volumes map[string]struct {
			Name     string
			External bool
		}
	}
	if err := json.Unmarshal(out, &model); err != nil {
		t.Fatal(err)
	}
	if len(model.Services) != 1 {
		t.Fatalf("want one independent service, got %d", len(model.Services))
	}
	service, ok := model.Services["runscaler-actions-cache"]
	if !ok {
		t.Fatal("cache service missing")
	}
	if service.Image != "ghcr.io/falcondev-oss/github-actions-cache-server@sha256:05c4d9e7169f525f2b74d412c4a081aeec5446a017ec88bae668e63ebfb16aa9" {
		t.Fatalf("unexpected unverified image: %q", service.Image)
	}
	if service.Platform != "linux/amd64" {
		t.Fatalf("runtime gate only verified amd64, got %q", service.Platform)
	}
	if service.ContainerName != "runscaler-actions-cache" || service.Restart != "unless-stopped" {
		t.Fatal("service must have an independent lifecycle")
	}
	if service.Labels["managed-by"] != "runscaler-cache" || service.Labels["runscaler.runner-id"] != "" {
		t.Fatalf("incorrect ownership labels: %v", service.Labels)
	}
	if len(service.Volumes) != 1 || service.Volumes[0].Type != "volume" || service.Volumes[0].Source != "runscaler-actions-cache-data" || service.Volumes[0].Target != "/app/.data" {
		t.Fatalf("incorrect data mount: %+v", service.Volumes)
	}
	volume := model.Volumes["runscaler-actions-cache-data"]
	if !volume.External || volume.Name != "runscaler-actions-cache-data" {
		t.Fatal("cache data must be an explicitly managed external volume")
	}
	if len(service.Ports) != 1 || service.Ports[0].HostIP != "192.168.50.10" || service.Ports[0].Published != "13000" || service.Ports[0].Target != 3000 {
		t.Fatalf("incorrect private interface binding: %+v", service.Ports)
	}
	for key, want := range map[string]string{
		"API_BASE_URL": "http://192.168.50.10:13000/", "HOST": "0.0.0.0", "PORT": "3000",
		"STORAGE_DRIVER": "filesystem", "DB_DRIVER": "sqlite", "SKIP_TOKEN_VALIDATION": "false",
		"CACHE_MAX_SIZE_BYTES": "1073741824", "CACHE_CLEANUP_OLDER_THAN_DAYS": "30",
		"MANAGEMENT_API_KEY": "controlled-test-only-key",
	} {
		if got := service.Environment[key]; got != want {
			t.Errorf("environment %s = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{"CACHE_FILESYSTEM_MAX_USAGE_PERCENT", "ACTIONS_RUNTIME_TOKEN", "GITHUB_TOKEN", "RUNNER_TOKEN", "CUSTOM_ACTIONS_RESULTS_URL"} {
		if _, exists := service.Environment[key]; exists {
			t.Errorf("job/runner credential or endpoint %s leaked into service config", key)
		}
	}
	if len(service.Healthcheck) != 0 {
		t.Fatal("distroless service must not assume a shell/curl healthcheck")
	}
}

func TestLocalCacheDeploymentRequiresOperatorInputs(t *testing.T) {
	for _, key := range []string{"CACHE_BIND_IP", "CACHE_ADVERTISE_URL", "CACHE_MAX_SIZE_BYTES", "CACHE_MANAGEMENT_KEY"} {
		t.Run(key, func(t *testing.T) {
			out, err := renderLocalCacheDeployment(t, key)
			if err == nil || !strings.Contains(string(out), key) {
				t.Fatalf("missing %s must fail rendering, err=%v output=%s", key, err, out)
			}
		})
	}
}
