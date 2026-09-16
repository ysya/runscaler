// Package pathtrust decides whether only root can change a file.
package pathtrust

import (
	"fmt"
	"io/fs"
)

// Problem explains why someone other than root could change a file with this
// owner and mode, or returns "" when only root can. Group write is never
// accepted: a POSIX ACL mask shows up in the group bits, and the root group
// can have other members.
func Problem(uid uint32, mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return "is a symlink"
	case uid != 0:
		return fmt.Sprintf("is owned by uid %d, not root", uid)
	case mode.Perm()&0o020 != 0:
		return "is writable by its group"
	case mode.Perm()&0o002 != 0:
		return "is writable by others"
	}
	return ""
}
