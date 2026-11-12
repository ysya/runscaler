package config

import (
	"strings"
	"testing"
)

func TestValidationErrorsCollectIndependentNestedProblems(t *testing.T) {
	ss := validScaleSetConfig()
	ss.Token = ""
	ss.GitHubApp = &GitHubAppConfig{}
	var scaleSetProblems []string
	for _, err := range ss.ValidationErrors() {
		scaleSetProblems = append(scaleSetProblems, err.Error())
	}
	joined := strings.Join(scaleSetProblems, "\n")
	for _, want := range []string{"github-app.client-id", "github-app.installation-id", "github-app requires exactly one"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q from %q", want, joined)
		}
	}
	if err := ss.Validate(); err == nil || !strings.Contains(err.Error(), "github-app.client-id") {
		t.Fatalf("runtime first error = %v", err)
	}

	cfg := Config{
		LogLevel: "info", LogFormat: "text",
		ActionsCache: ActionsCacheConfig{Enabled: true, ListenAddress: "bad", DataDir: "relative", MaxSizeBytes: -1},
		Disk:         DiskConfig{MinFree: "bad", TargetFree: "also-bad", MaxTier: 9},
	}
	var globalProblems []string
	for _, err := range cfg.GlobalValidationErrors() {
		globalProblems = append(globalProblems, err.Error())
	}
	joined = strings.Join(globalProblems, "\n")
	for _, want := range []string{"actions-cache.listen-address", "actions-cache.data-dir", "actions-cache.max-size-bytes", "disk: min-free", "disk: target-free", "disk: max-tier"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q from %q", want, joined)
		}
	}
	if err := cfg.ValidateGlobal(); err == nil || !strings.Contains(err.Error(), "actions-cache.listen-address") {
		t.Fatalf("runtime first global error = %v", err)
	}
}
