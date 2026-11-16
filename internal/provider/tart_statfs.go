//go:build darwin || linux

package provider

import "syscall"

// statfsFreeBytes is the space available to an unprivileged user on the
// filesystem that holds path.
func statfsFreeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
