package provider

import (
	"context"
	"errors"
	"fmt"
)

// ErrBelowCloneFloor marks a clone refused by a CloneFloor: the filesystem
// of TART_HOME has too little free space, or its free space cannot be read.
var ErrBelowCloneFloor = errors.New("clone floor")

// CloneFloor is the free-space floor every `tart clone` of runscaler must
// pass (see [tart] clone-min-free-gb). The zero value is no floor.
type CloneFloor struct {
	// MinFreeBytes refuses a clone while the filesystem that holds
	// TART_HOME has fewer bytes free (0 = no floor).
	MinFreeBytes uint64
	// FreeBytes reads the free bytes of a path; nil means statfs.
	// Tests inject a fake.
	FreeBytes func(path string) (uint64, error)
}

// Check returns an error wrapping ErrBelowCloneFloor when the filesystem of
// the Tart home (resolved as TartHomeDir(home): [tart].home, then
// $TART_HOME, then ~/.tart, the same place `tart clone` writes) is below the
// floor, or when its free space cannot be read (fail closed). It never reads
// the disk without a floor.
func (f CloneFloor) Check(home string) error {
	if f.MinFreeBytes == 0 {
		return nil
	}
	dir, err := TartHomeDir(home)
	if err != nil {
		return fmt.Errorf("%w: cannot resolve TART_HOME: %v", ErrBelowCloneFloor, err)
	}
	read := f.FreeBytes
	if read == nil {
		read = statfsFreeBytes
	}
	free, err := read(dir)
	if err != nil {
		return fmt.Errorf("%w: cannot read free space of %s: %v", ErrBelowCloneFloor, dir, err)
	}
	if free < f.MinFreeBytes {
		return fmt.Errorf("%w: %s has %d bytes free, below the floor of %d bytes; refusing to clone", ErrBelowCloneFloor, dir, free, f.MinFreeBytes)
	}
	return nil
}

// CloneTartVM is the one `tart clone` of runscaler: it checks floor on the
// Tart home and only then clones image to name. Every clone caller (runner
// VMs, startup guest inspection, the cache prune's mark-used clone) goes
// through it, so none of them can bypass the floor.
func CloneTartVM(ctx context.Context, runner CommandRunner, home string, floor CloneFloor, image, name string) error {
	if err := floor.Check(home); err != nil {
		return err
	}
	_, err := runner.Run(ctx, "tart", "clone", image, name)
	return err
}
