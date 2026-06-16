package versioncheck

import "testing"

func TestDownloadURLUsesRunnerAsset(t *testing.T) {
	got := DownloadURL("v1.2.3", "linux", "amd64")
	want := "https://github.com/ysya/runscaler/releases/download/v1.2.3/runner-linux-amd64.tar.gz"
	if got != want {
		t.Errorf("DownloadURL = %q, want %q", got, want)
	}
}
