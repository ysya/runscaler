package main

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
)

// homebrewBinDirs are Homebrew's bin directories on Apple Silicon and Intel
// Macs, where Tart is normally installed.
var homebrewBinDirs = []string{"/opt/homebrew/bin", "/usr/local/bin"}

// ensureHomebrewPath lets runner find tart when launchd starts it with
// PATH=/usr/bin:/bin:/usr/sbin:/sbin, without rewriting the plist or launchd's
// own configuration. Interactive shells already carry these directories, so
// they see no change.
func ensureHomebrewPath() {
	path := os.Getenv("PATH")
	if extended := homebrewPath(runtime.GOOS, path); extended != path {
		_ = os.Setenv("PATH", extended)
	}
}

// homebrewPath returns path with Homebrew's bin directories appended on macOS.
func homebrewPath(goos, path string) string {
	if goos != "darwin" {
		return path
	}
	return appendMissingDirs(path, homebrewBinDirs)
}

// appendMissingDirs appends each existing directory in dirs that path lacks.
// Appending keeps every command path already resolves unchanged.
func appendMissingDirs(path string, dirs []string) string {
	entries := filepath.SplitList(path)
	for _, dir := range dirs {
		if slices.Contains(entries, dir) {
			continue
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		entries = append(entries, dir)
		if path != "" {
			path += string(os.PathListSeparator)
		}
		path += dir
	}
	return path
}
