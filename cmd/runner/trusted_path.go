package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/ysya/runscaler/internal/pathtrust"
)

// fileStat is the ownership data the path checks need.
type fileStat struct {
	UID  uint32
	Mode fs.FileMode
}

type statFunc func(path string) (fileStat, error)

// lstatFile reads ownership without following a final symlink.
func lstatFile(path string) (fileStat, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return fileStat{}, err
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileStat{}, fmt.Errorf("stat %s: ownership unavailable on this platform", path)
	}
	return fileStat{UID: sys.Uid, Mode: info.Mode()}, nil
}

// untrustedPathError names a path someone other than root could change.
type untrustedPathError struct {
	Path   string
	Reason string
}

func (e *untrustedPathError) Error() string { return e.Path + " " + e.Reason }

// checkRootOnly requires that only root can change path itself.
func checkRootOnly(path string, stat statFunc) error {
	st, err := stat(path)
	if err != nil {
		return err
	}
	if reason := pathtrust.Problem(st.UID, st.Mode); reason != "" {
		return &untrustedPathError{Path: path, Reason: reason}
	}
	return nil
}

// checkRootOnlyChain applies checkRootOnly to an absolute path and every
// ancestor: whoever can write a parent directory can replace the child.
func checkRootOnlyChain(path string, stat statFunc) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s is not an absolute path", path)
	}
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		if err := checkRootOnly(p, stat); err != nil {
			return err
		}
		if p == "/" {
			return nil
		}
	}
}
