package main

import (
	"os"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func TestRunnerImagePullRequestsDoNotPublish(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/runner-image.yml")
	if err != nil {
		t.Fatal(err)
	}
	config := viper.New()
	config.SetConfigType("yaml")
	if err := config.ReadConfig(strings.NewReader(string(data))); err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name      string
				Uses      string
				Condition string `mapstructure:"if"`
				With      map[string]string
			}
		}
	}
	if err := config.Unmarshal(&workflow); err != nil {
		t.Fatal(err)
	}
	const guard = "github.event_name != 'pull_request'"
	foundBuild := false
	for _, step := range workflow.Jobs["build"].Steps {
		switch {
		case strings.HasPrefix(step.Uses, "docker/build-push-action@"):
			foundBuild = true
			if !strings.HasSuffix(step.With["outputs"], "push=${{ "+guard+" }}") {
				t.Fatal("image exporter must disable registry push on pull requests")
			}
			if !strings.HasPrefix(step.With["cache-to"], "${{ "+guard+" && ") || !strings.HasSuffix(step.With["cache-to"], " || '' }}") {
				t.Fatal("registry cache export must be empty on pull requests")
			}
		case strings.HasPrefix(step.Uses, "docker/login-action@"), strings.HasPrefix(step.Uses, "actions/upload-artifact@"), step.Name == "Export digest":
			if step.Condition != guard {
				t.Fatalf("publication-only step %q/%q must skip pull requests", step.Name, step.Uses)
			}
		}
	}
	if !foundBuild {
		t.Fatal("runner image validation build missing")
	}
}
