package main

import (
	"path/filepath"
	"testing"
)

func TestAppendMissingDirs(t *testing.T) {
	armBrew := t.TempDir()
	intelBrew := t.TempDir()
	missing := filepath.Join(t.TempDir(), "missing")
	launchd := "/usr/bin:/bin:/usr/sbin:/sbin"

	tests := []struct {
		name string
		path string
		dirs []string
		want string
	}{
		{
			name: "appends after launchd's default so system commands keep priority",
			path: launchd,
			dirs: []string{armBrew, intelBrew},
			want: launchd + ":" + armBrew + ":" + intelBrew,
		},
		{
			name: "leaves a directory already on PATH where it is",
			path: armBrew + ":" + launchd,
			dirs: []string{armBrew, intelBrew},
			want: armBrew + ":" + launchd + ":" + intelBrew,
		},
		{
			name: "skips directories that do not exist",
			path: launchd,
			dirs: []string{missing, armBrew},
			want: launchd + ":" + armBrew,
		},
		{
			// A leading separator would add an empty entry, which means the
			// current directory.
			name: "adds no empty entry to an empty PATH",
			path: "",
			dirs: []string{armBrew},
			want: armBrew,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := appendMissingDirs(tt.path, tt.dirs); got != tt.want {
				t.Errorf("appendMissingDirs(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
