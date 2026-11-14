package actionscache

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	entitypb "github.com/ysya/runscaler/internal/actionscache/pb/results/entities/v1"
)

type transportFunc func(*http.Request) (*http.Response, error)

func TestPrincipalReadableScopesPreservesPriorityTiesAndInput(t *testing.T) {
	var grants []Grant
	var writable, readable []string
	for i := range 48 {
		scope := "scope-" + strconv.Itoa(i)
		permission := int64(i % 4)
		grants = append(grants, Grant{Scope: scope, Permission: permission})
		switch permission {
		case 3:
			writable = append(writable, scope)
		case 1:
			readable = append(readable, scope)
		}
	}
	before := slices.Clone(grants)
	got := (Principal{Grants: grants}).ReadableScopes()
	want := append(writable, readable...)
	if !slices.Equal(got, want) {
		t.Fatalf("ReadableScopes() = %v, want %v", got, want)
	}
	if !slices.Equal(grants, before) {
		t.Fatal("ReadableScopes mutated the principal's grants")
	}
	for _, empty := range [][]Grant{nil, {}} {
		if got := (Principal{Grants: empty}).ReadableScopes(); got != nil {
			t.Fatalf("empty ReadableScopes() = %v, want nil", got)
		}
	}
}

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jwksResponse(key *rsa.PrivateKey, kid string) string {
	n := base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes())
	data, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid, "n": n, "e": e}}})
	return string(data)
}

func signFixture(t *testing.T, key *rsa.PrivateKey, kid string, overrides jwt.MapClaims) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss": "https://issuer.example", "aud": "cache-test", "exp": time.Now().Add(time.Hour).Unix(),
		"nbf": time.Now().Add(-time.Minute).Unix(), "repository_id": "fixture-repo",
		"ac": `[{"Scope":"refs/heads/main","Permission":3},{"Scope":"refs/heads/base","Permission":1}]`,
	}
	for k, v := range overrides {
		claims[k] = v
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = kid
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestVerifierRejectsInvalidTokenAndMetadataForgery(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://issuer.example/keys" {
			t.Errorf("unexpected JWKS URL %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(jwksResponse(key, "key1"))), Header: make(http.Header)}, nil
	})}
	verifier, err := NewVerifier(VerifierConfig{Issuer: "https://issuer.example", JWKSURL: "https://issuer.example/keys", Audiences: []string{"cache-test"}, Client: client})
	if err != nil {
		t.Fatal(err)
	}
	valid := signFixture(t, key, "key1", nil)
	if _, err := verifier.Verify(context.Background(), valid); err != nil {
		t.Fatalf("valid token: %v", err)
	}
	for name, raw := range map[string]string{
		"wrong issuer":       signFixture(t, key, "key1", jwt.MapClaims{"iss": "https://other.example"}),
		"expired":            signFixture(t, key, "key1", jwt.MapClaims{"exp": time.Now().Add(-time.Hour).Unix()}),
		"future":             signFixture(t, key, "key1", jwt.MapClaims{"nbf": time.Now().Add(time.Hour).Unix()}),
		"wrong audience":     signFixture(t, key, "key1", jwt.MapClaims{"aud": "other"}),
		"invalid signature":  signFixture(t, other, "key1", nil),
		"missing repository": signFixture(t, key, "key1", jwt.MapClaims{"repository_id": ""}),
		"invalid access":     signFixture(t, key, "key1", jwt.MapClaims{"ac": `[{"Scope":"refs/heads/main","Permission":7}]`}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := verifier.Verify(context.Background(), raw); err == nil {
				t.Fatal("accepted invalid token")
			}
		})
	}
	principal, err := verifier.Verify(context.Background(), valid)
	if err != nil {
		t.Fatal(err)
	}
	if principal.RepositoryID != "fixture-repo" || principal.FirstWritableScope() != "refs/heads/main" || !principal.CanRead("refs/heads/base") {
		t.Fatalf("wrong principal: %+v", principal)
	}
	if err := principal.AuthorizeMetadata(&entitypb.CacheMetadata{RepositoryId: 42}); err == nil {
		t.Fatal("forged repository metadata accepted")
	}
	if err := principal.AuthorizeMetadata(&entitypb.CacheMetadata{Scope: []*entitypb.CacheScope{{Scope: "refs/heads/admin", Permission: 3}}}); err == nil {
		t.Fatal("forged scope metadata accepted")
	}
	if err := principal.AuthorizeMetadata(&entitypb.CacheMetadata{Scope: []*entitypb.CacheScope{{Scope: "refs/heads/base", Permission: 2}}}); err == nil {
		t.Fatal("metadata write escalation accepted")
	}
}

func TestVerifierReadOnlyAndKeyRotation(t *testing.T) {
	first, _ := rsa.GenerateKey(rand.Reader, 2048)
	second, _ := rsa.GenerateKey(rand.Reader, 2048)
	current, kid := first, "first"
	client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(jwksResponse(current, kid))), Header: make(http.Header)}, nil
	})}
	verifier, err := NewVerifier(VerifierConfig{Issuer: "https://issuer.example", JWKSURL: "https://issuer.example/keys", Client: client})
	if err != nil {
		t.Fatal(err)
	}
	principal, err := verifier.Verify(context.Background(), signFixture(t, first, "first", jwt.MapClaims{"ac": `[{"Scope":"refs/heads/main","Permission":1}]}"`}))
	if err == nil {
		t.Fatalf("malformed access accepted: %+v", principal)
	}
	principal, err = verifier.Verify(context.Background(), signFixture(t, first, "first", jwt.MapClaims{"ac": `[{"Scope":"refs/heads/main","Permission":1}]`}))
	if err != nil {
		t.Fatal(err)
	}
	if principal.FirstWritableScope() != "" || !principal.CanRead("refs/heads/main") {
		t.Fatalf("read-only grant elevated: %+v", principal)
	}
	current, kid = second, "second"
	if _, err := verifier.Verify(context.Background(), signFixture(t, second, "second", nil)); err != nil {
		t.Fatalf("key rotation failed: %v", err)
	}
}

func TestVerifierBoundsUnknownKeyRefresh(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	requests := 0
	client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(jwksResponse(key, "known"))), Header: make(http.Header)}, nil
	})}
	verifier, err := NewVerifier(VerifierConfig{Issuer: "https://issuer.example", JWKSURL: "https://issuer.example/keys", Client: client})
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, kid := range []string{"unknown-a", "unknown-b"} {
		if _, err := verifier.Verify(context.Background(), signFixture(t, key, kid, nil)); err == nil {
			t.Fatal("unknown key accepted")
		}
	}
	if requests != 2 {
		t.Fatalf("unbounded JWKS fetches: %d", requests)
	}
}

func TestVerifierThrottlesFailedJWKSRefresh(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	requests := 0
	failing := false
	client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		requests++
		if failing {
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("outage")), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(jwksResponse(key, "known"))), Header: make(http.Header)}, nil
	})}
	verifier, err := NewVerifier(VerifierConfig{Issuer: "https://issuer.example", JWKSURL: "https://issuer.example/keys", Client: client})
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	verifier.fetched = time.Now().Add(-11 * time.Minute)
	failing = true
	for i := 0; i < 2; i++ {
		if _, err := verifier.Verify(context.Background(), signFixture(t, key, "known", nil)); err == nil {
			t.Fatal("accepted stale key after failed refresh")
		}
	}
	if requests != 2 {
		t.Fatalf("unbounded failed JWKS refreshes: %d", requests)
	}
}
