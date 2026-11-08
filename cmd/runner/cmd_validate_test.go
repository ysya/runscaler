package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/actions/scaleset"

	"github.com/ysya/runscaler/internal/config"
)

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
