package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"syscall"
	"testing"
)

func TestUpdateRestartNotice_WarnsWhenRunningVersionDiffers(t *testing.T) {
	target := &installationTarget{Scope: scopeUser, BinaryPath: "/opt/runner tools/runner", ConfigPath: "/home/ci/config $literal.toml"}
	notice := updateRestartNotice("0.5.0", "0.4.1", target, "runner validate --config x", nil)
	for _, want := range []string{"0.4.1", "0.5.0", "'/opt/runner tools/runner' service restart --scope user"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice must name both versions and the command to apply the new one; missing %q in:\n%s", want, notice)
		}
	}
	if strings.Contains(notice, "safely") || strings.Contains(notice, "sudo") {
		t.Fatalf("restart overpromises or escalates user service: %s", notice)
	}
}

func TestUpdateRestartNotice_QuietWhenNoActionIsNeeded(t *testing.T) {
	for _, tt := range []struct {
		name      string
		installed string
		running   string
	}{
		{"same version", "0.5.0", "0.5.0"},
		{"optional tag prefix", "v0.5.0", "0.5.0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := updateRestartNotice(tt.installed, tt.running, &installationTarget{Scope: scopeUser, BinaryPath: "/opt/runner"}, "", nil); got != "" {
				t.Errorf("updateRestartNotice() = %q, want empty", got)
			}
		})
	}
}

func TestUpdateUnknownRunningVersionDoesNotImplyStoppedService(t *testing.T) {
	target := &installationTarget{Scope: scopeSystem, BinaryPath: "/usr/local/bin/runner", ConfigPath: "/etc/runner/config.toml"}
	got := updateRestartNotice("v2.0.0", "", target, "sudo runner validate --config /etc/runner/config.toml", nil)
	for _, want := range []string{"could not be verified", "If the service is running", "--scope system", "drain", "stop timeout"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
	if strings.Contains(got, "not running") {
		t.Fatal("unavailable health was treated as proof the service stopped")
	}
}

func TestUpdateBinaryOnlyNoticeDoesNotControlUnselectedService(t *testing.T) {
	got := updateRestartNotice("v2.0.0", "v1.0.0", nil, "", nil)
	if !strings.Contains(got, "No service was selected") || !strings.Contains(got, "manually") {
		t.Fatalf("binary-only guidance=%s", got)
	}
	if strings.Contains(got, "service restart") || strings.Contains(got, "--scope") {
		t.Fatalf("unselected service command printed: %s", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestFetchRunningVersion(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "http://127.0.0.1:9090/healthz" {
			t.Errorf("health URL = %s", req.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"version":"0.4.1"}`)),
			Header:     make(http.Header),
		}, nil
	})}

	if got := fetchRunningVersion(context.Background(), "127.0.0.1", 9090, client); got != "0.4.1" {
		t.Errorf("fetchRunningVersion() = %q, want 0.4.1", got)
	}
}

func TestFetchRunningVersion_FailuresAreQuiet(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("not running")
	})}
	if got := fetchRunningVersion(context.Background(), "127.0.0.1", 8080, client); got != "" {
		t.Errorf("fetchRunningVersion() = %q, want empty on connection failure", got)
	}
	if got := fetchRunningVersion(context.Background(), "127.0.0.1", 0, client); got != "" {
		t.Errorf("disabled health endpoint returned %q", got)
	}
}

func TestUpdateFailureSuggestsSudoOnPermissionErrors(t *testing.T) {
	denied := fmt.Errorf("failed to create temp file in /usr/local/bin: %w",
		&fs.PathError{Op: "open", Path: "/usr/local/bin/.runner-update-1", Err: syscall.EACCES})
	if err := updateFailure(denied, "/usr/local/bin/runner", "/opt/new-runner"); !strings.Contains(err.Error(), "/opt/new-runner") || !strings.Contains(err.Error(), "--binary-path /usr/local/bin/runner") || !errors.Is(err, fs.ErrPermission) {
		t.Errorf("permission failure = %v", err)
	}
	if err := updateFailure(errors.New("checksum mismatch"), "/usr/local/bin/runner", "/opt/new-runner"); strings.Contains(err.Error(), "sudo") {
		t.Errorf("unrelated failure suggested sudo: %v", err)
	}
}
