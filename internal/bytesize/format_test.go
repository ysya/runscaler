package bytesize

import "testing"

func TestFormat(t *testing.T) {
	for _, tt := range []struct {
		input uint64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1048576, "1.0 MiB"},
		{1073741824, "1.0 GiB"},
	} {
		if got := Format(tt.input); got != tt.want {
			t.Errorf("Format(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
