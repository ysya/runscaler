package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/ysya/runscaler/internal/config"
)

func TestValidateReportsIndependentLocalErrorsBeforeConnectivity(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`
log-level = "invalid"
log-format = "text"
health-port = 65536

[[scaleset]]
url = "https://github.com/example-org"
name = "first"
token = "synthetic-test-token"
max-runners = 0
runner-image = ""

[[scaleset]]
url = "https://github.com/example-org"
name = "second"
token = "synthetic-test-token"
max-runners = 0
`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &cobra.Command{}
	c.Flags().String("config", path, "")
	var out bytes.Buffer
	c.SetOut(&out)
	if err := runValidate(c, nil); err == nil {
		t.Fatal("invalid local config passed validation")
	}
	for _, want := range []string{"log-level", "health-port", `scaleset[0] "first": max-runners`, `scaleset[0] "first": runner-image`, `scaleset[1] "second"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("validation omitted %q:\n%s", want, &out)
		}
	}
	if strings.Contains(out.String(), "Docker connectivity") || strings.Contains(out.String(), "GitHub API") {
		t.Fatalf("validation probed external services despite local errors:\n%s", &out)
	}
}

func TestValidatePreflightIsOptIn(t *testing.T) {
	flag := validateCmd.Flags().Lookup("preflight")
	if flag == nil {
		t.Fatal("validate has no --preflight flag")
	}
	if flag.DefValue != "false" {
		t.Fatalf("--preflight default = %q, want false", flag.DefValue)
	}
}

func TestOptionalImagePreflightCallsPreparationOnlyWhenRequested(t *testing.T) {
	command := &cobra.Command{}
	command.Flags().Bool("preflight", false, "")
	var out bytes.Buffer
	command.SetOut(&out)
	docker := validScaleSet("docker", 0)
	tart := validScaleSet("tart", 0)
	tart.Provider = "tart"
	sets := []config.ScaleSetConfig{docker, tart}
	var calls []string
	pullDocker := func(_ context.Context, ss config.ScaleSetConfig) error {
		calls = append(calls, "docker:"+ss.ScaleSetName)
		return nil
	}
	ensureTart := func(_ context.Context, ss config.ScaleSetConfig) error {
		calls = append(calls, "tart:"+ss.ScaleSetName)
		return nil
	}
	if err := runOptionalImagePreflight(command, sets, pullDocker, ensureTart); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 || out.Len() != 0 {
		t.Fatalf("default validation prepared images: calls=%q output=%q", calls, &out)
	}
	if err := command.Flags().Set("preflight", "true"); err != nil {
		t.Fatal(err)
	}
	if err := runOptionalImagePreflight(command, sets, pullDocker, ensureTart); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "docker:docker,tart:tart" || !strings.Contains(out.String(), "Preparing runner images") {
		t.Fatalf("explicit preflight calls=%q output=%q", calls, &out)
	}
}

func TestPrepareRunnerImagesOnlyCallsEachUniqueTarget(t *testing.T) {
	dockerA := validScaleSet("docker-a", 0)
	dockerA.RunnerImage = "example/runner:latest"
	dockerB := dockerA
	dockerB.ScaleSetName = "docker-b"
	dockerC := dockerA
	dockerC.ScaleSetName = "docker-c"
	dockerC.Docker.Platform = "linux/arm64"
	tartA := validScaleSet("tart-a", 0)
	tartA.Provider = "tart"
	tartA.RunnerImage = "example/tart:latest"
	tartA.Tart.Home = "/tmp/tart-a"
	tartB := tartA
	tartB.ScaleSetName = "tart-b"
	tartC := tartA
	tartC.ScaleSetName = "tart-c"
	tartC.Tart.Home = "/tmp/tart-c"
	var calls []string
	err := prepareRunnerImages(context.Background(), []config.ScaleSetConfig{dockerA, dockerB, dockerC, tartA, tartB, tartC},
		func(_ context.Context, ss config.ScaleSetConfig) error {
			calls = append(calls, "docker:"+ss.Docker.Socket+":"+ss.RunnerImage+":"+ss.Docker.Platform)
			return nil
		},
		func(_ context.Context, ss config.ScaleSetConfig) error {
			calls = append(calls, "tart:"+ss.Tart.Home+":"+ss.RunnerImage)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"docker:" + dockerA.Docker.Socket + ":example/runner:latest:",
		"docker:" + dockerA.Docker.Socket + ":example/runner:latest:linux/arm64",
		"tart:/tmp/tart-a:example/tart:latest",
		"tart:/tmp/tart-c:example/tart:latest",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("image preparation calls = %q, want %q", calls, want)
	}
}

func TestRequireDockerAPIVersion(t *testing.T) {
	for _, tc := range []struct {
		serverVersion string
		clientVersion string
		wantErr       bool
	}{
		{serverVersion: "", clientVersion: "1.56", wantErr: true},
		{serverVersion: "1.48", clientVersion: "1.48", wantErr: true},
		{serverVersion: "1.49", clientVersion: "1.48", wantErr: true},
		{serverVersion: "1.49", clientVersion: "1.49", wantErr: false},
		{serverVersion: "1.56", clientVersion: "1.56", wantErr: false},
	} {
		t.Run(tc.serverVersion+"/"+tc.clientVersion, func(t *testing.T) {
			err := requireDockerAPIVersion(tc.serverVersion, tc.clientVersion)
			if (err != nil) != tc.wantErr {
				t.Fatalf("requireDockerAPIVersion(%q, %q) error = %v, want error %v", tc.serverVersion, tc.clientVersion, err, tc.wantErr)
			}
		})
	}
}

func TestValidateGitHubAccessUsesTheConfiguredRunnerGroup(t *testing.T) {
	ss := validScaleSet("builders-set", 0)
	ss.RunnerGroup = "builders"
	client := &doctorProbeClient{fakeScaleSetAPI: fakeScaleSetAPI{
		groups:   map[string]int{"builders": 42},
		existing: map[int]*scaleset.RunnerScaleSet{42: {ID: 7, Name: "builders-set"}},
	}}
	var out bytes.Buffer
	err := validateGitHubAccess(context.Background(), &out, []config.ScaleSetConfig{ss}, func(config.ScaleSetConfig) (scaleSetAPI, error) { return client, nil })
	if err != nil {
		t.Fatalf("validateGitHubAccess() = %v\n%s", err, &out)
	}
	if len(client.lookedUp) != 1 || client.lookedUp[0] != 42 {
		t.Fatalf("queried groups %v, want the configured group 42", client.lookedUp)
	}
	if !strings.Contains(out.String(), "scale set registered") {
		t.Fatalf("report does not say the scale set is registered:\n%s", &out)
	}
}

func TestValidateGitHubAccessPrintsOnlySafeErrorCategories(t *testing.T) {
	ss := validScaleSet("broken", 0)
	var out bytes.Buffer
	err := validateGitHubAccess(context.Background(), &out, []config.ScaleSetConfig{ss}, func(config.ScaleSetConfig) (scaleSetAPI, error) {
		return &doctorProbeClient{err: errors.New("401 for https://api.github.com with ghp_syntheticsecret")}, nil
	})
	if err == nil {
		t.Fatal("a failed probe passed validation")
	}
	if strings.Contains(out.String(), "ghp_syntheticsecret") || strings.Contains(err.Error(), "ghp_syntheticsecret") {
		t.Fatalf("raw SDK error text reached the output:\n%s", &out)
	}
	if !strings.Contains(out.String(), "request failed") || !strings.Contains(out.String(), "token has the correct scopes") {
		t.Fatalf("missing the safe category or the credential hint:\n%s", &out)
	}
}

func TestValidateGitHubAccessExplainsRateLimits(t *testing.T) {
	ss := validScaleSet("limited", 0)
	var out bytes.Buffer
	err := validateGitHubAccess(context.Background(), &out, []config.ScaleSetConfig{ss}, func(config.ScaleSetConfig) (scaleSetAPI, error) {
		limited := &scaleset.RateLimitError{StatusCode: 403, Kind: scaleset.RateLimitPrimary, RetryAfter: 20*time.Minute - 300*time.Millisecond}
		return &doctorProbeClient{err: fmt.Errorf("failed to get runner scale set: %w", limited)}, nil
	})
	if err == nil {
		t.Fatal("a rate-limited probe passed validation")
	}
	if !strings.Contains(out.String(), "HTTP 403 rate limited (primary); retry after 20m0s") || !strings.Contains(out.String(), "rate-limiting these credentials") {
		t.Fatalf("missing the rate-limit category or hint:\n%s", &out)
	}
	if strings.Contains(out.String(), "correct scopes") {
		t.Fatalf("a rate limit was reported as a scope problem:\n%s", &out)
	}
}
