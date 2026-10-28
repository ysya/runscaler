package config

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/actions/scaleset"
	"github.com/hashicorp/go-retryablehttp"
)

// GitHubAppConfig stores references to a private key, never its PEM contents.
// Like PAT credentials, this configuration is local to a single scale set.
type GitHubAppConfig struct {
	ClientID       string `mapstructure:"client-id"`
	InstallationID int64  `mapstructure:"installation-id"`
	PrivateKeyFile string `mapstructure:"private-key-file"`
	PrivateKeyEnv  string `mapstructure:"private-key-env"`
}

var appKeyEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (ss *ScaleSetConfig) validateAuthentication() error {
	app := ss.GitHubApp
	if app == nil {
		if ss.Token == "" {
			return fmt.Errorf("token is required unless github-app is configured")
		}
		return nil
	}
	if ss.Token != "" {
		return fmt.Errorf("token and github-app cannot be configured together")
	}
	if strings.TrimSpace(app.ClientID) == "" {
		return fmt.Errorf("github-app.client-id is required")
	}
	if app.InstallationID <= 0 {
		return fmt.Errorf("github-app.installation-id must be greater than zero")
	}
	if (app.PrivateKeyFile == "") == (app.PrivateKeyEnv == "") {
		return fmt.Errorf("github-app requires exactly one of private-key-file or private-key-env")
	}
	if app.PrivateKeyFile != "" && !filepath.IsAbs(app.PrivateKeyFile) {
		return fmt.Errorf("github-app.private-key-file must be an absolute path")
	}
	if app.PrivateKeyEnv != "" && !appKeyEnvName.MatchString(app.PrivateKeyEnv) {
		return fmt.Errorf("github-app.private-key-env must be an environment variable name")
	}
	if u, err := url.Parse(ss.RegistrationURL); err == nil && strings.HasPrefix(strings.Trim(u.Path, "/"), "enterprises/") {
		return fmt.Errorf("GitHub App authentication is unavailable for enterprise-level runners; use a PAT")
	}
	return nil
}

const maxAppPrivateKeySize = 64 * 1024

func (a GitHubAppConfig) privateKey() (string, error) {
	var data []byte
	if a.PrivateKeyEnv != "" {
		value := os.Getenv(a.PrivateKeyEnv)
		if strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("github-app private key environment variable %q is empty or unset", a.PrivateKeyEnv)
		}
		if len(value) > maxAppPrivateKeySize {
			return "", fmt.Errorf("github-app private key exceeds 64 KiB")
		}
		data = []byte(value)
	} else {
		info, err := os.Stat(a.PrivateKeyFile)
		if err != nil {
			return "", fmt.Errorf("cannot read github-app private key file: %w", err)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("github-app private key file must be a regular file")
		}
		f, err := os.Open(a.PrivateKeyFile)
		if err != nil {
			return "", fmt.Errorf("cannot open github-app private key file: %w", err)
		}
		defer f.Close()
		data, err = io.ReadAll(io.LimitReader(f, maxAppPrivateKeySize+1))
		if err != nil {
			return "", fmt.Errorf("cannot read github-app private key file: %w", err)
		}
	}
	if len(data) > maxAppPrivateKeySize {
		return "", fmt.Errorf("github-app private key exceeds 64 KiB")
	}
	block, rest := pem.Decode(data)
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 {
		return "", fmt.Errorf("github-app private key must contain one unencrypted RSA PEM key")
	}
	var parsed any
	var err error
	switch block.Type {
	case "RSA PRIVATE KEY":
		parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	default:
		return "", fmt.Errorf("github-app private key must be an unencrypted RSA PEM key")
	}
	if _, ok := parsed.(*rsa.PrivateKey); err != nil || !ok {
		return "", fmt.Errorf("github-app private key must be a valid RSA PEM key")
	}
	return string(data), nil
}

// NewScalesetClient selects the configured App or PAT credentials. The SDK
// handles JWT signing and credential exchange when API requests need them.
func (ss *ScaleSetConfig) NewScalesetClient(logger *slog.Logger, options ...scaleset.HTTPOption) (*scaleset.Client, error) {
	if err := ss.validateAuthentication(); err != nil {
		return nil, err
	}
	httpClient := retryablehttp.NewClient()
	httpClient.Logger = nil
	opts := []scaleset.HTTPOption{scaleset.WithLogger(logger), scaleset.WithRetryableHTTPClint(httpClient)}
	opts = append(opts, options...)
	var client *scaleset.Client
	var err error
	if ss.GitHubApp != nil {
		key, keyErr := ss.GitHubApp.privateKey()
		if keyErr != nil {
			return nil, keyErr
		}
		client, err = scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
			GitHubConfigURL: ss.RegistrationURL,
			GitHubAppAuth: scaleset.GitHubAppAuth{
				ClientID: ss.GitHubApp.ClientID, InstallationID: ss.GitHubApp.InstallationID, PrivateKey: key,
			},
		}, opts...)
	} else {
		client, err = scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL: ss.RegistrationURL, PersonalAccessToken: ss.Token,
		}, opts...)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create scaleset client: %w", err)
	}
	return client, nil
}
