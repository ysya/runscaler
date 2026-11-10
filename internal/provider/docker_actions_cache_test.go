package provider

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/shell"
)

func TestDockerProviderActionsCacheOptIn(t *testing.T) {
	for _, tc := range []struct {
		name, url string
		targets   []string
	}{
		{name: "disabled"},
		{name: "enabled", url: "http://cache.example:3000/"},
		{name: "enabled with ownership setup", url: "http://cache.example:3000/", targets: []string{"/cache/with space", "/cache/a'$(id)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dc := config.DockerConfig{ActionsCacheURL: tc.url}
			for i, target := range tc.targets {
				dc.CacheVolumes = append(dc.CacheVolumes, string(rune('a'+i))+"-cache:"+target)
			}
			p, client := newConfigDockerProvider(dc)
			if _, err := p.StartInstance(context.Background(), "cache-test", "test-jit"); err != nil {
				t.Fatal(err)
			}
			created := client.createCalls[0].config
			if tc.url == "" {
				if !slices.Equal(created.Env, []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=test-jit"}) || !slices.Equal(created.Cmd, []string{"/home/runner/run.sh"}) {
					t.Fatalf("disabled behavior changed: env=%v cmd=%v", created.Env, created.Cmd)
				}
				return
			}
			if !slices.Contains(created.Env, "CUSTOM_ACTIONS_RESULTS_URL="+tc.url) {
				t.Fatal("local endpoint was not injected")
			}
			if len(created.Cmd) != 3 || created.Cmd[0] != "sh" || created.Cmd[1] != "-c" {
				t.Fatalf("enabled mode needs a guarded shell command: %v", created.Cmd)
			}
			script := created.Cmd[2]
			check := strings.Index(script, "/usr/local/bin/runner-local-cache-check && exec /home/runner/run.sh")
			if check < 0 {
				t.Fatalf("listener not gated by checker success: %q", script)
			}
			for _, target := range tc.targets {
				own := strings.Index(script, "fix_own "+shell.Quote(target)+";")
				if own < 0 || own >= check {
					t.Fatalf("ownership setup must precede checker: %q", script)
				}
			}
			if strings.Contains(script, tc.url) || strings.Contains(script, "test-jit") {
				t.Fatal("runtime values must remain environment data, not shell code")
			}
		})
	}
}

func TestDockerProviderActionsCacheURLStaysEnvironmentData(t *testing.T) {
	endpoint := "http://cache.example/a'$(id)/"
	p, client := newConfigDockerProvider(config.DockerConfig{ActionsCacheURL: endpoint})
	if _, err := p.StartInstance(context.Background(), "cache-test", "test-jit"); err != nil {
		t.Fatal(err)
	}
	created := client.createCalls[0].config
	if !slices.Contains(created.Env, "CUSTOM_ACTIONS_RESULTS_URL="+endpoint) {
		t.Fatal("endpoint was rewritten or not injected")
	}
	if strings.Contains(strings.Join(created.Cmd, " "), endpoint) {
		t.Fatal("endpoint interpolated into shell command")
	}
}

func TestLocalCacheCheckerRequiresEndpoint(t *testing.T) {
	script, err := filepath.Abs("../../images/actions-runner/check-local-cache.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("local cache checker missing: %v", err)
	}
	command := exec.Command(script)
	command.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "endpoint") {
		t.Fatalf("checker must reject absent endpoint before listener start: output=%q err=%v", out, err)
	}
}
