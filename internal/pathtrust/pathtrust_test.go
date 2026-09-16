package pathtrust

import (
	"io/fs"
	"testing"
)

func TestProblem(t *testing.T) {
	tests := []struct {
		name string
		uid  uint32
		mode fs.FileMode
		want string
	}{
		{name: "root-only file", uid: 0, mode: 0o755},
		{name: "root-only directory", uid: 0, mode: fs.ModeDir | 0o755},
		{name: "user owned", uid: 1000, mode: 0o755, want: "is owned by uid 1000, not root"},
		{name: "group writable", uid: 0, mode: 0o775, want: "is writable by its group"},
		{name: "others writable", uid: 0, mode: 0o757, want: "is writable by others"},
		{name: "symlink", uid: 0, mode: fs.ModeSymlink | 0o777, want: "is a symlink"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Problem(tt.uid, tt.mode); got != tt.want {
				t.Errorf("Problem() = %q, want %q", got, tt.want)
			}
		})
	}
}
