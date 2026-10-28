package config

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/actions/scaleset"
	"github.com/hashicorp/go-retryablehttp"
)

func appScaleSet() ScaleSetConfig {
	return ScaleSetConfig{
		RegistrationURL: "https://github.com/example-org", ScaleSetName: "builders",
		MaxRunners: 1, RunnerImage: DefaultRunnerImage, Provider: DefaultProvider,
		GitHubApp: &GitHubAppConfig{ClientID: "Iv1.synthetic", InstallationID: 123, PrivateKeyEnv: "RUNNER_TEST_APP_KEY"},
	}
}

func TestGitHubAppAuthValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*ScaleSetConfig)
		want   string
	}{
		{"valid App", func(*ScaleSetConfig) {}, ""},
		{"missing client ID", func(ss *ScaleSetConfig) { ss.GitHubApp.ClientID = "" }, "client-id"},
		{"missing installation", func(ss *ScaleSetConfig) { ss.GitHubApp.InstallationID = 0 }, "installation-id"},
		{"negative installation", func(ss *ScaleSetConfig) { ss.GitHubApp.InstallationID = -1 }, "installation-id"},
		{"missing key source", func(ss *ScaleSetConfig) { ss.GitHubApp.PrivateKeyEnv = "" }, "private-key"},
		{"ambiguous key source", func(ss *ScaleSetConfig) { ss.GitHubApp.PrivateKeyFile = "/tmp/synthetic.pem" }, "exactly one"},
		{"relative key path", func(ss *ScaleSetConfig) { ss.GitHubApp.PrivateKeyEnv = ""; ss.GitHubApp.PrivateKeyFile = "app.pem" }, "absolute"},
		{"PAT conflict", func(ss *ScaleSetConfig) { ss.Token = "synthetic-pat" }, "cannot"},
		{"enterprise scope", func(ss *ScaleSetConfig) { ss.RegistrationURL = "https://github.com/enterprises/example" }, "enterprise"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ss := appScaleSet()
			tc.change(&ss)
			err := ss.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestGitHubAppIgnoresAmbientPATButRejectsExplicitPAT(t *testing.T) {
	t.Setenv("RUNNER_TOKEN", "synthetic-ambient-pat")
	t.Setenv("RUNSCALER_TOKEN", "synthetic-legacy-pat")
	ss := appScaleSet()
	ss.resolveEnvToken()
	if ss.Token != "" {
		t.Fatal("App auth acquired an ambient PAT")
	}
	ss.Token = "env:RUNNER_TOKEN"
	ss.resolveEnvToken()
	if err := ss.Validate(); err == nil {
		t.Fatal("explicit PAT and App must conflict")
	}
}

func TestIncompleteAppConfigurationNeverFallsBackToPAT(t *testing.T) {
	t.Setenv("RUNNER_TOKEN", "synthetic-ambient-pat")
	for _, source := range []string{
		`github-app.client-id = ""`,
		`github-app.installation-id = 0`,
		`github-app.private-key-env = ""`,
		`[github-app]`,
		`github-app = {}`,
	} {
		cfg := loadTOML(t, source)
		ss := cfg.ResolveScaleSets()[0]
		if ss.GitHubApp == nil || ss.Token != "" {
			t.Fatal("incomplete App configuration silently selected PAT")
		}
		if _, err := ss.NewScalesetClient(slog.New(slog.DiscardHandler)); err == nil {
			t.Fatal("incomplete App configuration accepted")
		}
	}
}

func TestEmptyAppInScaleSetNeverFallsBackToPAT(t *testing.T) {
	t.Setenv("RUNNER_TOKEN", "synthetic-ambient-pat")
	cfg := loadTOML(t, `
[[scaleset]]
url = "https://github.com/example-org"
name = "builders"
[scaleset.github-app]
`)
	ss := cfg.ResolveScaleSets()[0]
	if ss.GitHubApp == nil || ss.Token != "" {
		t.Fatal("empty scale-set App table selected an ambient PAT")
	}
	if _, err := ss.NewScalesetClient(slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("empty App table accepted")
	}
}

type authTransport func(*http.Request) (*http.Response, error)

func (f authTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestScalesetClientSelectsAppSigningOrPAT(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	keyFile := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUNNER_TEST_APP_KEY", string(keyPEM))
	for _, mode := range []string{"App environment", "App file", "PAT"} {
		t.Run(mode, func(t *testing.T) {
			ss := appScaleSet()
			if mode == "App file" {
				ss.GitHubApp.PrivateKeyEnv = ""
				ss.GitHubApp.PrivateKeyFile = keyFile
			}
			if mode == "PAT" {
				ss.GitHubApp = nil
				ss.Token = "synthetic-pat"
			}
			requests := 0
			httpClient := retryablehttp.NewClient()
			httpClient.Logger = nil
			transport := authTransport(func(req *http.Request) (*http.Response, error) {
				requests++
				if mode == "PAT" {
					if req.URL.Path != "/orgs/example-org/actions/runners/registration-token" || req.Header.Get("Authorization") != "Bearer synthetic-pat" {
						t.Fatal("PAT client used the wrong authentication endpoint or credential")
					}
				} else {
					if req.URL.Path != "/app/installations/123/access_tokens" {
						t.Fatalf("unexpected auth path: %s", req.URL.Path)
					}
					parts := strings.Split(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "), ".")
					if len(parts) != 3 {
						t.Fatal("App client did not send a JWT")
					}
					payload, err := base64.RawURLEncoding.DecodeString(parts[1])
					if err != nil {
						t.Fatal("invalid JWT encoding")
					}
					var claims struct {
						Issuer string `json:"iss"`
					}
					if err := json.Unmarshal(payload, &claims); err != nil || claims.Issuer != "Iv1.synthetic" {
						t.Fatal("incorrect App JWT issuer")
					}
					signature, err := base64.RawURLEncoding.DecodeString(parts[2])
					if err != nil {
						t.Fatal("invalid JWT signature encoding")
					}
					digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
					if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
						t.Fatal("App JWT was not signed by the configured key")
					}
				}
				// Stop after the auth request: exercise the real SDK signer without any external network.
				return &http.Response{StatusCode: 400, Status: "400 Bad Request", Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader("{}")), ContentLength: 2}, nil
			})
			client, err := ss.NewScalesetClient(slog.New(slog.DiscardHandler), scaleset.WithRetryableHTTPClint(httpClient))
			if err != nil {
				t.Fatal(err)
			}
			// The SDK initializes a concrete *http.Transport during construction.
			// Replace the external transport only after that initialization.
			httpClient.HTTPClient.Transport = transport
			if _, err := client.GetRunnerGroupByName(context.Background(), "default"); err == nil {
				t.Fatal("expected synthetic backend rejection")
			}
			if requests != 1 {
				t.Fatalf("auth requests = %d, want 1", requests)
			}
		})
	}
}

func TestGitHubAppKeyFailuresDoNotDiscloseContents(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"unset", ""},
		{"malformed", "synthetic-private-material-must-not-appear"},
		{"oversized", strings.Repeat("synthetic-key-material", 4096)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RUNNER_TEST_APP_KEY", tc.value)
			ss := appScaleSet()
			_, err := ss.NewScalesetClient(slog.New(slog.DiscardHandler))
			if err == nil {
				t.Fatal("invalid key source accepted")
			}
			if tc.value != "" && strings.Contains(err.Error(), tc.value) {
				t.Fatal("private key material leaked in error")
			}
		})
	}
}

func TestGitHubAppRejectsPrivateMaterialAsEnvironmentReference(t *testing.T) {
	ss := appScaleSet()
	ss.GitHubApp.PrivateKeyEnv = "-----BEGIN PRIVATE KEY-----\nsynthetic-private-material\n-----END PRIVATE KEY-----"
	_, err := ss.NewScalesetClient(slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("private-key-env must be a variable name, not key contents")
	}
	if strings.Contains(err.Error(), "synthetic-private-material") {
		t.Fatal("invalid environment reference leaked key contents")
	}
}

func TestGitHubAppPrivateKeyFileValidation(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	nonRSA, err := x509.MarshalPKCS8PrivateKey(edKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		data    []byte
		wantErr bool
	}{
		{"PKCS8 RSA", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), false},
		{"non-RSA", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: nonRSA}), true},
		{"oversized", []byte(strings.Repeat("synthetic", 8192)), true},
		{"empty", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "app.pem")
			if err := os.WriteFile(path, tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			ss := appScaleSet()
			ss.GitHubApp.PrivateKeyEnv = ""
			ss.GitHubApp.PrivateKeyFile = path
			_, err := ss.NewScalesetClient(slog.New(slog.DiscardHandler))
			if (err != nil) != tc.wantErr {
				t.Fatalf("client error = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && len(tc.data) > 0 && strings.Contains(err.Error(), string(tc.data)) {
				t.Fatal("private file contents leaked in error")
			}
		})
	}
	for _, path := range []string{filepath.Join(t.TempDir(), "missing.pem"), t.TempDir()} {
		ss := appScaleSet()
		ss.GitHubApp.PrivateKeyEnv = ""
		ss.GitHubApp.PrivateKeyFile = path
		if _, err := ss.NewScalesetClient(slog.New(slog.DiscardHandler)); err == nil {
			t.Fatal("missing or non-regular private key accepted")
		}
	}
}

func TestGitHubAppCredentialsAreNotInherited(t *testing.T) {
	cfg := loadTOML(t, `
github-app.client-id = "Iv1.top-level"
github-app.installation-id = 100
github-app.private-key-env = "TOP_LEVEL_KEY"
[[scaleset]]
url = "https://github.com/org-a"
name = "app-runners"
github-app.client-id = "Iv1.org-a"
github-app.installation-id = 200
github-app.private-key-env = "ORG_A_KEY"
[[scaleset]]
url = "https://github.com/org-b"
name = "pat-runners"
token = "synthetic-pat"
`)
	sets := cfg.ResolveScaleSets()
	if sets[0].GitHubApp.ClientID != "Iv1.org-a" || sets[0].GitHubApp.InstallationID != 200 {
		t.Fatal("explicit App credentials did not survive load")
	}
	if sets[1].GitHubApp != nil {
		t.Fatal("another scale set inherited App credentials")
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "github-app") {
		t.Fatalf("ignored top-level App not reported: %v", cfg.Warnings)
	}
}
