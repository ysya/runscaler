package bytesize

import "testing"

func TestParseThreshold(t *testing.T) {
	tests := []struct {
		in      string
		percent float64
		bytes   uint64
		wantErr bool
	}{
		{in: "10%", percent: 10},
		{in: "20GB", bytes: 20 * 1024 * 1024 * 1024},
		{in: "512MB", bytes: 512 * 1024 * 1024},
		{in: "0%", percent: 0},
		{in: "101%", wantErr: true},
		{in: "-5%", wantErr: true},
		{in: "abc", wantErr: true},
		{in: "", wantErr: true},
		{in: "NaN%", wantErr: true},
		{in: "nan%", wantErr: true},
		{in: "Inf%", wantErr: true},
		{in: "+Inf%", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseThreshold(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseThreshold(%q) expected error, got %+v", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseThreshold(%q) error: %v", tt.in, err)
			continue
		}
		if got.Percent != tt.percent || got.Bytes != tt.bytes {
			t.Errorf("ParseThreshold(%q) = %+v, want percent=%v bytes=%v",
				tt.in, got, tt.percent, tt.bytes)
		}
	}
}

func TestThresholdBytesOf(t *testing.T) {
	pct, _ := ParseThreshold("10%")
	if got := pct.BytesOf(1000); got != 100 {
		t.Errorf("10%% of 1000 = %d, want 100", got)
	}
	abs, _ := ParseThreshold("20GB")
	if got := abs.BytesOf(1000); got != 20*1024*1024*1024 {
		t.Errorf("absolute threshold must ignore total, got %d", got)
	}
	full, _ := ParseThreshold("100%")
	if got := full.BytesOf(500); got != 500 {
		t.Errorf("100%% of 500 = %d, want 500 (upper boundary must still be accepted)", got)
	}
}
