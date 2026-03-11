package versioncheck

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsNewer(t *testing.T) {
	tests := []struct {
		name   string
		local  string
		remote string
		want   bool
	}{
		{"same version", "0.2.5", "0.2.5", false},
		{"same with v prefix", "v0.2.5", "v0.2.5", false},
		{"mixed v prefix", "0.2.5", "v0.2.5", false},
		{"mixed v prefix reverse", "v0.2.5", "0.2.5", false},
		{"remote is newer", "0.2.5", "0.3.0", true},
		{"remote is older", "0.3.0", "0.2.5", true}, // IsNewer only checks inequality
		{"dev build", "dev", "0.2.5", true},
		{"empty local", "", "0.2.5", true},
		{"dev with v remote", "dev", "v0.2.5", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsNewer(tt.local, tt.remote)
			if got != tt.want {
				t.Errorf("IsNewer(%q, %q) = %v, want %v", tt.local, tt.remote, got, tt.want)
			}
		})
	}
}

func TestLatest(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		release := ReleaseInfo{
			TagName: "v0.3.0",
			HTMLURL: "https://github.com/ysya/runscaler/releases/tag/v0.3.0",
		}

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(release)
		}))
		defer srv.Close()

		origTransport := http.DefaultTransport
		http.DefaultTransport = &testTransport{url: srv.URL, transport: origTransport}
		defer func() { http.DefaultTransport = origTransport }()

		got, err := Latest(context.Background())
		if err != nil {
			t.Fatalf("Latest() error = %v", err)
		}
		if got.TagName != release.TagName {
			t.Errorf("TagName = %q, want %q", got.TagName, release.TagName)
		}
		if got.HTMLURL != release.HTMLURL {
			t.Errorf("HTMLURL = %q, want %q", got.HTMLURL, release.HTMLURL)
		}
	})

	t.Run("server error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		origTransport := http.DefaultTransport
		http.DefaultTransport = &testTransport{url: srv.URL, transport: origTransport}
		defer func() { http.DefaultTransport = origTransport }()

		_, err := Latest(context.Background())
		if err == nil {
			t.Fatal("Latest() expected error for 500 response")
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := Latest(ctx)
		if err == nil {
			t.Fatal("Latest() expected error for cancelled context")
		}
	})
}

// testTransport redirects all requests to the test server URL.
type testTransport struct {
	url       string
	transport http.RoundTripper
}

func (t *testTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = "http"
	req.URL.Host = t.url[len("http://"):]
	return t.transport.RoundTrip(req)
}
