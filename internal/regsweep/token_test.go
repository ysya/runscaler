package regsweep

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestAppTokenSourceExchangesAndCachesInstallationToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	var mu sync.Mutex
	now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	exchanges := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/42/access_tokens" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		claims := &jwt.RegisteredClaims{}
		_, err := jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) { return &key.PublicKey, nil },
			jwt.WithValidMethods([]string{"RS256"}), jwt.WithTimeFunc(func() time.Time { mu.Lock(); defer mu.Unlock(); return now }))
		if err != nil || claims.Issuer != "Iv1.test" {
			t.Errorf("JWT invalid: %v, issuer %q", err, claims.Issuer)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"organization_self_hosted_runners":"read"`) {
			t.Errorf("token request does not ask for read-only runner access: %s", body)
		}
		mu.Lock()
		exchanges++
		n := exchanges
		expires := now.Add(time.Hour)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_" + string(rune('a'+n)), "expires_at": expires})
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	src, err := NewAppTokenSource("Iv1.test", 42, pemKey, base, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	src.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	first, err := src.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	advance(50 * time.Minute)
	if again, _ := src.Token(context.Background()); again != first || exchanges != 1 {
		t.Fatalf("token not cached: %q then %q after %d exchanges", first, again, exchanges)
	}
	advance(6 * time.Minute) // within tokenRefreshMargin of expiry
	if renewed, _ := src.Token(context.Background()); renewed == first || exchanges != 2 {
		t.Fatalf("token not renewed near expiry: %q, %d exchanges", renewed, exchanges)
	}
}

func TestAppTokenSourceErrorCarriesNoBody(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "secret-body", http.StatusForbidden)
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	src, _ := NewAppTokenSource("Iv1.test", 42, pemKey, base, srv.Client())
	_, err := src.Token(context.Background())
	if err == nil || strings.Contains(err.Error(), "secret-body") || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("error = %v", err)
	}
}
