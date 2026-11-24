package provider

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/pkg/authconfig"
)

// Keep test pulls away from the operator's login and platform credential helpers.
func isolatedDockerConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", dir)
	t.Setenv("PATH", dir)
	return dir
}

func TestDockerPullSelectsCredentialHelperByRegistry(t *testing.T) {
	dir := isolatedDockerConfig(t)
	cfg := `{"credHelpers":{"https://index.docker.io/v1/":"fixture","registry.example:5000":"fixture","broken.example":"fixture"}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	const script = `#!/bin/sh
[ "$1" = get ] || exit 1
case "$(/bin/cat)" in
  https://index.docker.io/v1/) echo '{"Username":"<token>","Secret":"hub-login"}' ;;
  registry.example:5000) echo '{"Username":"user","Secret":"port-login"}' ;;
  *) echo "fixture-private-output"; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker-credential-fixture"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	m := newDinDFake()
	m.images = map[string]bool{}
	for _, tc := range []struct{ image, domain, password, identityToken string }{
		{"docker:29-dind", "docker.io", "", "hub-login"},
		{"registry.example:5000/runner:1", "registry.example:5000", "port-login", ""},
		{"registry.example:5001/runner:1", "", "", ""},
	} {
		pull, err := PullDockerImage(t.Context(), m, tc.image, nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = pull.Close()
		encoded := m.pullAuth[tc.image]
		if tc.domain == "" && encoded != "" {
			t.Fatal("a registry at a different port received credentials")
		}
		cfg, err := authconfig.Decode(encoded)
		if err != nil || cfg.ServerAddress != tc.domain || cfg.Password != tc.password || cfg.IdentityToken != tc.identityToken {
			t.Errorf("unexpected registry login for %s", tc.image)
		}
	}
	_, err := PullDockerImage(t.Context(), m, "broken.example/runner", nil)
	if err == nil || !strings.Contains(err.Error(), "broken.example") || strings.Contains(err.Error(), "fixture-private-output") {
		t.Fatal("helper failure must identify the registry without exposing helper output")
	}
	if len(m.pulls) != 3 {
		t.Fatal("helper failure reached the Docker daemon")
	}
}

func TestDockerConfigForServiceWithoutHomeEnvironment(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", "")
	t.Setenv("HOME", "")
	account, err := user.Current()
	if err != nil || account.HomeDir == "" {
		t.Skip("test user has no home directory")
	}
	if got := dockerConfigDir(); got != filepath.Join(account.HomeDir, ".docker") {
		t.Errorf("did not select the service account's Docker config directory")
	}
}
