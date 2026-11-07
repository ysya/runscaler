package main

import (
	"os"
	"path/filepath"
	"strings"
)

// writeFileAtomic replaces a file only after its private temporary copy is
// flushed. Config and service definitions share this write contract.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".runner-write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func shellQuotePath(path string) string {
	return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'"
}

func sameFilePath(a, b string) bool {
	aAbs, aErr := filepath.Abs(a)
	bAbs, bErr := filepath.Abs(b)
	if aErr == nil && bErr == nil {
		return filepath.Clean(aAbs) == filepath.Clean(bAbs)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
