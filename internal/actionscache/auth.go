package actionscache

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	entitypb "github.com/ysya/runscaler/internal/actionscache/pb/results/entities/v1"
)

type Grant struct {
	Scope      string
	Permission int64
}

type Principal struct {
	RepositoryID string
	Grants       []Grant
}

func (p Principal) FirstWritableScope() string {
	for _, grant := range p.Grants {
		if grant.Permission&2 != 0 {
			return grant.Scope
		}
	}
	return ""
}

func (p Principal) CanRead(scope string) bool {
	for _, grant := range p.Grants {
		if grant.Scope == scope && grant.Permission&1 != 0 {
			return true
		}
	}
	return false
}

func (p Principal) ReadableScopes() []string {
	grants := append([]Grant(nil), p.Grants...)
	sort.SliceStable(grants, func(i, j int) bool { return grants[i].Permission > grants[j].Permission })
	var scopes []string
	for _, grant := range grants {
		if grant.Permission&1 != 0 {
			scopes = append(scopes, grant.Scope)
		}
	}
	return scopes
}

// AuthorizeMetadata ensures request metadata cannot widen a signed grant.
// Cache identity is always derived from the verified token, never metadata.
func (p Principal) AuthorizeMetadata(metadata *entitypb.CacheMetadata) error {
	if metadata == nil {
		return nil
	}
	if metadata.RepositoryId != 0 && strconv.FormatInt(metadata.RepositoryId, 10) != p.RepositoryID {
		return errors.New("repository metadata conflicts with token")
	}
	for _, requested := range metadata.Scope {
		if requested == nil || requested.Scope == "" || requested.Permission < 0 || requested.Permission > 3 {
			return errors.New("invalid scope metadata")
		}
		allowed := false
		for _, grant := range p.Grants {
			if grant.Scope == requested.Scope && requested.Permission&^grant.Permission == 0 {
				allowed = true
				break
			}
		}
		if !allowed {
			return errors.New("scope metadata conflicts with token")
		}
	}
	return nil
}

type VerifierConfig struct {
	Issuer    string
	JWKSURL   string
	Audiences []string
	Client    *http.Client
}

type Verifier struct {
	cfg                VerifierConfig
	client             *http.Client
	mu                 sync.Mutex
	keys               map[string]*rsa.PublicKey
	fetched            time.Time
	lastUnknownRefresh time.Time
}

func NewVerifier(cfg VerifierConfig) (*Verifier, error) {
	issuer, err := url.Parse(cfg.Issuer)
	if err != nil || issuer.Hostname() == "" || (issuer.Scheme != "https" && issuer.Scheme != "http") {
		return nil, errors.New("invalid token issuer")
	}
	jwks, err := url.Parse(cfg.JWKSURL)
	if err != nil || jwks.Hostname() == "" || jwks.Scheme != issuer.Scheme || jwks.User != nil || jwks.RawQuery != "" || jwks.Fragment != "" {
		return nil, errors.New("invalid JWKS URL")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	if cfg.Client != nil {
		*client = *cfg.Client
		if client.Timeout == 0 {
			client.Timeout = 5 * time.Second
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Verifier{cfg: cfg, client: client}, nil
}

func (v *Verifier) Verify(ctx context.Context, raw string) (Principal, error) {
	if len(raw) == 0 || len(raw) > 16*1024 {
		return Principal{}, errors.New("invalid token")
	}
	opts := []jwt.ParserOption{jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(v.cfg.Issuer), jwt.WithExpirationRequired(), jwt.WithJSONNumber()}
	if len(v.cfg.Audiences) > 0 {
		opts = append(opts, jwt.WithAudience(v.cfg.Audiences...))
	}
	claims := jwt.MapClaims{}
	token, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		kid, ok := token.Header["kid"].(string)
		if !ok || len(kid) == 0 || len(kid) > 256 {
			return nil, errors.New("invalid key ID")
		}
		return v.keyFor(ctx, kid)
	}, opts...)
	if err != nil || token == nil || !token.Valid {
		return Principal{}, errors.New("invalid token")
	}
	return principalFromClaims(claims)
}

func (v *Verifier) keyFor(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if key := v.keys[kid]; key != nil && time.Since(v.fetched) < 10*time.Minute {
		return key, nil
	}
	if len(v.keys) > 0 && v.keys[kid] == nil {
		if time.Since(v.lastUnknownRefresh) < 30*time.Second {
			return nil, errors.New("unknown key ID")
		}
		v.lastUnknownRefresh = time.Now()
	}
	if err := v.refresh(ctx); err != nil {
		return nil, err
	}
	key := v.keys[kid]
	if key == nil {
		return nil, errors.New("unknown key ID")
	}
	return key, nil
}

func (v *Verifier) Bootstrap(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.refresh(ctx)
}

func (v *Verifier) refresh(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, v.cfg.JWKSURL, nil)
	if err != nil {
		return errors.New("invalid JWKS request")
	}
	response, err := v.client.Do(request)
	if err != nil {
		return errors.New("JWKS unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("JWKS unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return errors.New("invalid JWKS")
	}
	var set struct {
		Keys []struct{ Kty, Use, Alg, Kid, N, E string }
	}
	if err := json.Unmarshal(data, &set); err != nil || len(set.Keys) == 0 || len(set.Keys) > 32 {
		return errors.New("invalid JWKS")
	}
	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, item := range set.Keys {
		if item.Kty != "RSA" || (item.Use != "" && item.Use != "sig") || (item.Alg != "" && item.Alg != "RS256") || item.Kid == "" || len(item.Kid) > 256 {
			continue
		}
		nBytes, nErr := base64.RawURLEncoding.DecodeString(item.N)
		eBytes, eErr := base64.RawURLEncoding.DecodeString(item.E)
		if nErr != nil || eErr != nil || len(nBytes) < 256 || len(nBytes) > 1024 || len(eBytes) == 0 || len(eBytes) > 4 {
			continue
		}
		e := new(big.Int).SetBytes(eBytes)
		if !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 {
			continue
		}
		keys[item.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: int(e.Int64())}
	}
	if len(keys) == 0 {
		return errors.New("no usable JWKS keys")
	}
	v.keys, v.fetched = keys, time.Now()
	return nil
}

func principalFromClaims(claims jwt.MapClaims) (Principal, error) {
	var repo string
	switch value := claims["repository_id"].(type) {
	case string:
		repo = value
	case json.Number:
		if n, err := value.Int64(); err == nil && n > 0 {
			repo = strconv.FormatInt(n, 10)
		}
	}
	if repo == "" || strings.TrimSpace(repo) != repo || len(repo) > 128 {
		return Principal{}, errors.New("invalid repository identity")
	}
	rawAccess, ok := claims["ac"].(string)
	if !ok || len(rawAccess) > 32*1024 {
		return Principal{}, errors.New("invalid access grants")
	}
	var grantValues []struct {
		Scope      string
		Permission int64
	}
	if err := json.Unmarshal([]byte(rawAccess), &grantValues); err != nil || len(grantValues) == 0 || len(grantValues) > 64 {
		return Principal{}, errors.New("invalid access grants")
	}
	grants := make([]Grant, 0, len(grantValues))
	for _, value := range grantValues {
		if value.Scope == "" || len(value.Scope) > 512 || value.Permission < 1 || value.Permission > 3 {
			return Principal{}, errors.New("invalid access grant")
		}
		grants = append(grants, Grant{Scope: value.Scope, Permission: value.Permission})
	}
	return Principal{RepositoryID: repo, Grants: grants}, nil
}
