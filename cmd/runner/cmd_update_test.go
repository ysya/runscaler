package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestUpdateRestartNotice_WarnsWhenRunningVersionDiffers(t *testing.T) {
	notice := updateRestartNotice("0.5.0", "0.4.1")
	for _, want := range []string{"0.4.1", "0.5.0", "runner service restart"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice must name both versions and the command to apply the new one; missing %q in:\n%s", want, notice)
		}
	}
}

func TestUpdateRestartNotice_QuietWhenNoActionIsNeeded(t *testing.T) {
	for _, tt := range []struct {
		name      string
		installed string
		running   string
	}{
		{"nothing running", "0.5.0", ""},
		{"same version", "0.5.0", "0.5.0"},
		{"optional tag prefix", "v0.5.0", "0.5.0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := updateRestartNotice(tt.installed, tt.running); got != "" {
				t.Errorf("updateRestartNotice() = %q, want empty", got)
			}
		})
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
