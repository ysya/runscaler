package main

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeStat(entries map[string]fileStat) statFunc {
	return func(path string) (fileStat, error) {
		st, ok := entries[path]
		if !ok {
			return fileStat{}, &fs.PathError{Op: "lstat", Path: path, Err: fs.ErrNotExist}
		}
		return st, nil
	}
}

func rootDir(perm fs.FileMode) fileStat {
	return fileStat{UID: 0, Mode: fs.ModeDir | perm}
}

func TestCheckRootOnlyChain(t *testing.T) {
	safe := map[string]fileStat{
		"/":                     rootDir(0o755),
		"/usr":                  rootDir(0o755),
		"/usr/local":            rootDir(0o755),
		"/usr/local/bin":        rootDir(0o755),
		"/usr/local/bin/runner": {UID: 0, Mode: 0o755},
	}
	with := func(path string, st fileStat) map[string]fileStat {
		entries := maps.Clone(safe)
		entries[path] = st
		return entries
	}
	tests := []struct {
		name       string
		entries    map[string]fileStat
		wantPath   string
		wantReason string
	}{
		{name: "root-owned chain", entries: safe},
		{name: "binary owned by a user", entries: with("/usr/local/bin/runner", fileStat{UID: 1000, Mode: 0o755}), wantPath: "/usr/local/bin/runner", wantReason: "owned by uid 1000"},
		{name: "group-writable binary", entries: with("/usr/local/bin/runner", fileStat{UID: 0, Mode: 0o775}), wantPath: "/usr/local/bin/runner", wantReason: "writable by its group"},
		{name: "others-writable ancestor", entries: with("/usr/local", rootDir(0o757)), wantPath: "/usr/local", wantReason: "writable by others"},
		{name: "user-owned ancestor", entries: with("/usr/local/bin", fileStat{UID: 1000, Mode: fs.ModeDir | 0o755}), wantPath: "/usr/local/bin", wantReason: "owned by uid 1000"},
		{name: "symlinked component", entries: with("/usr/local/bin/runner", fileStat{UID: 0, Mode: fs.ModeSymlink | 0o777}), wantPath: "/usr/local/bin/runner", wantReason: "is a symlink"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkRootOnlyChain("/usr/local/bin/runner", fakeStat(tt.entries))
			if tt.wantPath == "" {
				if err != nil {
					t.Fatalf("checkRootOnlyChain() = %v, want nil", err)
				}
				return
			}
			var untrusted *untrustedPathError
			if !errors.As(err, &untrusted) || untrusted.Path != tt.wantPath || !strings.Contains(untrusted.Reason, tt.wantReason) {
				t.Fatalf("checkRootOnlyChain() = %v, want %s %s", err, tt.wantPath, tt.wantReason)
			}
		})
	}
}

func TestCheckRootOnlyChainPropagatesStatErrors(t *testing.T) {
	err := checkRootOnlyChain("/usr/local/bin/runner", fakeStat(map[string]fileStat{}))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("checkRootOnlyChain() = %v, want fs.ErrNotExist", err)
	}
}

func TestCheckRootOnlyChainRejectsRelativePaths(t *testing.T) {
	if err := checkRootOnlyChain("bin/runner", fakeStat(nil)); err == nil {
		t.Fatal("relative path passed")
	}
}

func TestCheckRootOnlyChainRealFiles(t *testing.T) {
	// Any file others can write fails, whoever owns it and wherever it lives.
	writable := filepath.Join(t.TempDir(), "runner")
	if err := os.WriteFile(writable, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writable, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := checkRootOnlyChain(writable, lstatFile); err == nil {
		t.Fatalf("world-writable %s passed the check", writable)
	}

	sh, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	if st, err := lstatFile(sh); err != nil || st.UID != 0 {
		t.Skipf("%s is not root-owned on this host", sh)
	}
	if err := checkRootOnlyChain(sh, lstatFile); err != nil {
		t.Fatalf("system shell %s failed the check: %v", sh, err)
	}
}
