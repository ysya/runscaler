//go:build darwin || linux

package diskguard

import (
	"fmt"
	"syscall"
)

// FSStat is a point-in-time capacity snapshot for one filesystem. ID is
// stable across every path mounted on that filesystem, so callers group by
// ID (see Guard.statByFilesystem) rather than by path — usage is measured
// and reclaimed per filesystem, not per store.
type FSStat struct {
	ID         string
	TotalBytes uint64
	FreeBytes  uint64
}

// StatFor reports capacity for the filesystem containing path.
func StatFor(path string) (FSStat, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return FSStat{}, fmt.Errorf("statfs %q: %w", path, err)
	}

	// Bsize's width and signedness differ between darwin (uint32) and linux
	// (int64); converting through uint64 works for either without needing
	// to know which one we're compiling for.
	bsize := uint64(st.Bsize)
	return FSStat{
		// Fsid's field layout differs between darwin (Val [2]int32) and
		// linux (X__val [2]int32) — different field names, so it can't be
		// destructured the same way on both. Formatting the whole struct
		// sidesteps the difference entirely while still yielding a value
		// that is identical for two paths on the same filesystem and
		// differs across filesystems.
		ID:         fmt.Sprintf("%v", st.Fsid),
		TotalBytes: uint64(st.Blocks) * bsize,
		// Bavail (blocks available to unprivileged users), not Bfree
		// (which also counts root-reserved blocks) — the runner does not
		// run as root, so Bfree would overstate the space it can use.
		FreeBytes: uint64(st.Bavail) * bsize,
	}, nil
}
