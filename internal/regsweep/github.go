package regsweep

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// The scale set SDK keeps its GitHub REST helpers and token exchange
// private, and has no runner list call, so the sweep has its own small REST
// client. Errors carry only fixed text and HTTP status codes: never a
// response body, a URL with credentials, or a token.

const (
	perPage         = 100
	defaultMaxPages = 50
	// tokenRefreshMargin renews an installation token this long before
	// GitHub's expiry, so a sweep never starts with a token about to lapse.
	tokenRefreshMargin = 5 * time.Minute
	maxErrorBody       = 64 * 1024
)

// APIBaseURL maps a registration URL to its GitHub REST API root, as the
// scale set SDK does: api.github.com for github.com, api.<host> for
// *.ghe.com, and <host>/api/v3 for GitHub Enterprise Server.
func APIBaseURL(registrationURL string) (*url.URL, error) {
	u, err := url.Parse(strings.Trim(registrationURL, "/"))
	if err != nil || u.Host == "" {
		return nil, errors.New("invalid registration URL")
	}
	base := &url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/api/v3"}
	if isHostedGitHub(u.Host) {
		base.Host, base.Path = "api."+u.Host, ""
		if strings.EqualFold(u.Host, "www.github.com") {
			base.Host = "api.github.com"
		}
	}
	return base, nil
}

func isHostedGitHub(host string) bool {
	if _, forceGHES := os.LookupEnv("GITHUB_ACTIONS_FORCE_GHES"); forceGHES {
		return false
	}
	return strings.EqualFold(host, "github.com") ||
		strings.EqualFold(host, "www.github.com") ||
		strings.EqualFold(host, "github.localhost") ||
		strings.HasSuffix(strings.ToLower(host), ".ghe.com")
}

// TokenSource returns a credential for GitHub REST requests.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// StaticToken is a personal access token.
type StaticToken string

// Token returns the token.
func (t StaticToken) Token(context.Context) (string, error) {
	if t == "" {
		return "", errors.New("no token configured")
	}
	return string(t), nil
}

// AppTokenSource exchanges a GitHub App JWT for an installation token and
// caches the token until shortly before it expires. It asks for read access
// to organization self-hosted runners only: the sweep lists through REST and
// removes through the scale set API.
type AppTokenSource struct {
	clientID       string
	installationID int64
	key            *rsa.PrivateKey
	base           *url.URL
	client         *http.Client
	now            func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewAppTokenSource parses pemKey and returns a token source for the
// installation.
func NewAppTokenSource(clientID string, installationID int64, pemKey string, base *url.URL, client *http.Client) (*AppTokenSource, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(pemKey))
	if err != nil {
		return nil, errors.New("github-app private key is not a valid RSA PEM key")
	}
	if clientID == "" || installationID <= 0 {
		return nil, errors.New("github-app client ID and installation ID are required")
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &AppTokenSource{clientID: clientID, installationID: installationID, key: key, base: base, client: client, now: time.Now}, nil
}

// Token returns a cached installation token or exchanges a new one.
func (a *AppTokenSource) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if a.token != "" && now.Before(a.expires.Add(-tokenRefreshMargin)) {
		return a.token, nil
	}
	issued := now.Add(-time.Minute) // GitHub allows for clock drift this way
	appJWT, err := jwt.NewWithClaims(jwt.SigningMethodRS256, &jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(issued),
		ExpiresAt: jwt.NewNumericDate(issued.Add(9 * time.Minute)),
		Issuer:    a.clientID,
	}).SignedString(a.key)
	if err != nil {
		return "", errors.New("sign github-app JWT failed")
	}
	endpoint := a.base.JoinPath("app", "installations", strconv.FormatInt(a.installationID, 10), "access_tokens")
	body := []byte(`{"permissions":{"organization_self_hosted_runners":"read"}}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return "", errors.New("build installation token request failed")
	}
	setHeaders(req, appJWT)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("installation token request failed: %w", requestError(err))
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("installation token request failed: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&out); err != nil || out.Token == "" {
		return "", errors.New("installation token response could not be read")
	}
	a.token, a.expires = out.Token, out.ExpiresAt
	return a.token, nil
}

// GroupLister lists the runners of one organization runner group through
// GET /orgs/{org}/actions/runner-groups/{group_id}/runners. It resolves the
// group ID on first use and keeps it once resolved, so a failed lookup is
// retried by the next sweep instead of stopping job service at startup.
type GroupLister struct {
	base     *url.URL
	org      string
	resolve  func(ctx context.Context) (int, error)
	tokens   TokenSource
	client   *http.Client
	maxPages int

	mu      sync.Mutex
	groupID int
}

// NewGroupLister returns a lister for org's runner group that resolve
// names.
func NewGroupLister(base *url.URL, org string, resolve func(ctx context.Context) (int, error), tokens TokenSource, client *http.Client) *GroupLister {
	if client == nil {
		client = http.DefaultClient
	}
	return &GroupLister{base: base, org: org, resolve: resolve, tokens: tokens, client: client, maxPages: defaultMaxPages}
}

func (l *GroupLister) group(ctx context.Context) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.groupID > 0 {
		return l.groupID, nil
	}
	id, err := l.resolve(ctx)
	if err != nil {
		return 0, fmt.Errorf("resolve runner group: %w", err)
	}
	if id <= 0 {
		return 0, fmt.Errorf("resolve runner group: got ID %d", id)
	}
	l.groupID = id
	return id, nil
}

// ListRunners reads every page before it returns, so no caller acts on a
// list that its own removals could shift.
func (l *GroupLister) ListRunners(ctx context.Context) ([]Runner, error) {
	groupID, err := l.group(ctx)
	if err != nil {
		return nil, err
	}
	token, err := l.tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	endpoint := l.base.JoinPath("orgs", l.org, "actions", "runner-groups", strconv.Itoa(groupID), "runners")
	var all []Runner
	for page := 1; page <= l.maxPages; page++ {
		q := url.Values{"per_page": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)}}
		endpoint.RawQuery = q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return nil, errors.New("build runner list request failed")
		}
		setHeaders(req, token)
		resp, err := l.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("list runner group %d: %w", groupID, requestError(err))
		}
		var out struct {
			TotalCount int      `json:"total_count"`
			Runners    []Runner `json:"runners"`
		}
		status := resp.StatusCode
		var decodeErr error
		if status == http.StatusOK {
			decodeErr = json.NewDecoder(resp.Body).Decode(&out)
		}
		drain(resp)
		if status != http.StatusOK {
			return nil, fmt.Errorf("list runner group %d: HTTP %d", groupID, status)
		}
		if decodeErr != nil {
			return nil, fmt.Errorf("list runner group %d: response could not be read", groupID)
		}
		all = append(all, out.Runners...)
		if len(out.Runners) < perPage || len(all) >= out.TotalCount {
			return all, nil
		}
	}
	return nil, fmt.Errorf("list runner group %d: more than %d pages", groupID, l.maxPages)
}

func setHeaders(req *http.Request, credential string) {
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	_ = resp.Body.Close()
}

// requestError keeps a transport error's category without its text, which
// names the request URL.
func requestError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return errors.New("transport error")
	}
}
