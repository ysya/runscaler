package actionscache

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	cachepb "github.com/ysya/runscaler/internal/actionscache/pb/results/api/v1"
	"google.golang.org/protobuf/proto"
)

func handlerFixture(t *testing.T) (http.Handler, *Store, *rsa.PrivateKey, *int) {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	store, err := OpenStore(filepath.Join(t.TempDir(), "native"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	verifier, err := NewVerifier(VerifierConfig{Issuer: "https://issuer.example", JWKSURL: "https://issuer.example/keys", Audiences: []string{"cache-test"}, Client: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(jwksResponse(key, "key1"))), Header: make(http.Header)}, nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	upstreamCalls := new(int)
	handler, err := NewHandler(HandlerConfig{Store: store, Verifier: verifier, BaseURL: "http://cache.local:3000/", UpstreamURL: "https://results.example", UpstreamTransport: transportFunc(func(r *http.Request) (*http.Response, error) {
		*upstreamCalls++
		if r.URL.Host != "results.example" {
			t.Errorf("client-selected upstream: %s", r.URL.Host)
		}
		data, _ := io.ReadAll(r.Body)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data)), Header: make(http.Header)}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	return handler, store, key, upstreamCalls
}

func requestHandler(handler http.Handler, method, target, token string, body []byte, contentType string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func rpcJSON(t *testing.T, handler http.Handler, method, token string, body any) (int, map[string]any) {
	t.Helper()
	data, _ := json.Marshal(body)
	response := requestHandler(handler, http.MethodPost, "/twirp/github.actions.results.api.v1.CacheService/"+method, token, data, "application/json")
	var parsed map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("response JSON: %v: %s", err, response.Body.String())
	}
	return response.Code, parsed
}

func TestHandlerBlockRoundTripAndLocalRange(t *testing.T) {
	handler, _, key, upstreamCalls := handlerFixture(t)
	token := signFixture(t, key, "key1", nil)
	status, created := rpcJSON(t, handler, "CreateCacheEntry", token, map[string]any{"key": "archive", "version": "v1"})
	if status != 200 || created["ok"] != true {
		t.Fatalf("create: %d %+v", status, created)
	}
	upload, err := url.Parse(created["signed_upload_url"].(string))
	if err != nil || upload.Host != "cache.local:3000" {
		t.Fatalf("nonlocal upload: %s, %v", upload, err)
	}
	for _, part := range []struct{ id, body string }{{"c2Vjb25k", "world"}, {"Zmlyc3Q=", "hello "}} {
		block := *upload
		query := block.Query()
		query.Set("comp", "block")
		query.Set("blockid", part.id)
		block.RawQuery = query.Encode()
		response := requestHandler(handler, http.MethodPut, block.String(), "", []byte(part.body), "application/octet-stream")
		if response.Code != 201 || response.Header().Get("x-ms-request-id") == "" {
			t.Fatalf("put block: %d", response.Code)
		}
	}
	list := *upload
	query := list.Query()
	query.Set("comp", "blocklist")
	list.RawQuery = query.Encode()
	response := requestHandler(handler, http.MethodPut, list.String(), "", []byte(`<BlockList><Latest>Zmlyc3Q=</Latest><Latest>c2Vjb25k</Latest></BlockList>`), "application/xml")
	if response.Code != 201 {
		t.Fatalf("block list: %d %s", response.Code, response.Body.String())
	}
	status, finalized := rpcJSON(t, handler, "FinalizeCacheEntryUpload", token, map[string]any{"key": "archive", "version": "v1", "size_bytes": "11"})
	if status != 200 || finalized["ok"] != true {
		t.Fatalf("finalize: %d %+v", status, finalized)
	}
	status, lookup := rpcJSON(t, handler, "GetCacheEntryDownloadURL", token, map[string]any{"key": "missing", "restore_keys": []string{"arc"}, "version": "v1"})
	if status != 200 || lookup["ok"] != true || lookup["matched_key"] != "archive" {
		t.Fatalf("lookup: %d %+v", status, lookup)
	}
	download := lookup["signed_download_url"].(string)
	rangeRequest := httptest.NewRequest(http.MethodGet, download, nil)
	rangeRequest.Header.Set("Range", "bytes=4-8")
	rangeResponse := httptest.NewRecorder()
	handler.ServeHTTP(rangeResponse, rangeRequest)
	if rangeResponse.Code != 206 || rangeResponse.Body.String() != "o wor" {
		t.Fatalf("range: %d %q", rangeResponse.Code, rangeResponse.Body.String())
	}
	bad := download + "x"
	if response := requestHandler(handler, http.MethodGet, bad, "", nil, ""); response.Code != 403 {
		t.Fatalf("tampered capability: %d", response.Code)
	}
	if *upstreamCalls != 0 {
		t.Fatalf("cache payload proxied: %d", *upstreamCalls)
	}
}

func TestHandlerFailsCacheClosedAndProxiesArtifacts(t *testing.T) {
	handler, _, key, upstreamCalls := handlerFixture(t)
	token := signFixture(t, key, "key1", nil)
	for _, path := range []string{"/twirp/github.actions.results.api.v1.CacheService/Unknown", "/_apis/artifactcache/cache", "/devstoreaccount1/upload/unknown", "/download/unknown"} {
		response := requestHandler(handler, http.MethodPost, path, token, []byte("payload"), "application/json")
		if response.Code < 400 {
			t.Fatalf("cache path %s accepted: %d", path, response.Code)
		}
	}
	if *upstreamCalls != 0 {
		t.Fatalf("unknown cache path proxied: %d", *upstreamCalls)
	}
	response := requestHandler(handler, http.MethodPost, "https://evil.example/fixture-artifact", "", []byte("artifact"), "application/octet-stream")
	if response.Code != 200 || response.Body.String() != "artifact" || *upstreamCalls != 1 {
		t.Fatalf("artifact proxy: %d %q %d", response.Code, response.Body.String(), *upstreamCalls)
	}
	status, _ := rpcJSON(t, handler, "CreateCacheEntry", "", map[string]any{"key": "unauth", "version": "v1"})
	if status != 401 {
		t.Fatalf("missing token: %d", status)
	}
	readOnly := signFixture(t, key, "key1", jwt.MapClaims{"ac": `[{"Scope":"refs/heads/main","Permission":1}]`})
	status, _ = rpcJSON(t, handler, "CreateCacheEntry", readOnly, map[string]any{"key": "read-only", "version": "v1"})
	if status != 403 {
		t.Fatalf("read-only reserve: %d", status)
	}
	status, miss := rpcJSON(t, handler, "GetCacheEntryDownloadURL", token, map[string]any{"key": "missing", "version": "v1"})
	if status != 200 || miss["ok"] != false {
		t.Fatalf("miss response: %d %+v", status, miss)
	}
}

func TestHandlerProtobufAndSingleShotUpload(t *testing.T) {
	handler, _, key, upstreamCalls := handlerFixture(t)
	token := signFixture(t, key, "key1", nil)
	encoded, err := proto.Marshal(&cachepb.CreateCacheEntryRequest{Key: "single", Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	response := requestHandler(handler, http.MethodPost, cacheRPCPrefix+"CreateCacheEntry", token, encoded, "application/protobuf")
	var created cachepb.CreateCacheEntryResponse
	if response.Code != 200 || proto.Unmarshal(response.Body.Bytes(), &created) != nil || !created.Ok {
		t.Fatalf("protobuf create: %d %s", response.Code, response.Body.String())
	}
	upload := requestHandler(handler, http.MethodPut, created.SignedUploadUrl, "", []byte("single-shot"), "application/octet-stream")
	if upload.Code != 201 {
		t.Fatalf("single-shot upload: %d", upload.Code)
	}
	encoded, _ = proto.Marshal(&cachepb.FinalizeCacheEntryUploadRequest{Key: "single", Version: "v1", SizeBytes: 11})
	response = requestHandler(handler, http.MethodPost, cacheRPCPrefix+"FinalizeCacheEntryUpload", token, encoded, "application/protobuf")
	var finalized cachepb.FinalizeCacheEntryUploadResponse
	if response.Code != 200 || proto.Unmarshal(response.Body.Bytes(), &finalized) != nil || !finalized.Ok {
		t.Fatalf("protobuf finalize: %d %s", response.Code, response.Body.String())
	}
	status, found := rpcJSON(t, handler, "GetCacheEntryDownloadURL", token, map[string]any{"key": "single", "version": "v1"})
	if status != 200 || found["ok"] != true {
		t.Fatalf("lookup: %d %+v", status, found)
	}
	got := requestHandler(handler, http.MethodGet, found["signed_download_url"].(string), "", nil, "")
	if got.Code != 200 || got.Body.String() != "single-shot" {
		t.Fatalf("download: %d %q", got.Code, got.Body.String())
	}
	parsedURL, err := url.Parse(found["signed_download_url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	expired := handler.(*cacheHandler).signer.url("download", strings.TrimPrefix(parsedURL.Path, "/download/"), -time.Hour)
	if invalid := requestHandler(handler, http.MethodGet, expired, "", nil, ""); invalid.Code != 403 {
		t.Fatalf("expired capability: %d", invalid.Code)
	}
	if *upstreamCalls != 0 {
		t.Fatalf("single-shot payload proxied: %d", *upstreamCalls)
	}
}

func TestHandlerEnforcesBudgetAfterFinalization(t *testing.T) {
	handler, store, key, _ := handlerFixture(t)
	handler.(*cacheHandler).maxBytes = 12
	handler.(*cacheHandler).maxIdle = 30 * 24 * time.Hour
	token := signFixture(t, key, "key1", nil)
	for _, cacheKey := range []string{"old", "new"} {
		status, created := rpcJSON(t, handler, "CreateCacheEntry", token, map[string]any{"key": cacheKey, "version": "v1"})
		if status != 200 || created["ok"] != true {
			t.Fatalf("create %s: %d %+v", cacheKey, status, created)
		}
		response := requestHandler(handler, http.MethodPut, created["signed_upload_url"].(string), "", []byte("12345678"), "application/octet-stream")
		if response.Code != 201 {
			t.Fatalf("upload %s: %d", cacheKey, response.Code)
		}
		status, completed := rpcJSON(t, handler, "FinalizeCacheEntryUpload", token, map[string]any{"key": cacheKey, "version": "v1", "size_bytes": "8"})
		if status != 200 || completed["ok"] != true {
			t.Fatalf("finalize %s: %d %+v", cacheKey, status, completed)
		}
	}
	p := Principal{RepositoryID: "fixture-repo", Grants: []Grant{{Scope: "refs/heads/main", Permission: 3}}}
	if _, ok, err := store.Lookup(context.Background(), p, "old", nil, "v1"); err != nil || ok {
		t.Fatalf("old entry not evicted: %v, %v", ok, err)
	}
	if _, ok, err := store.Lookup(context.Background(), p, "new", nil, "v1"); err != nil || !ok {
		t.Fatalf("new entry missing: %v, %v", ok, err)
	}
}

type blockedBody struct {
	once    sync.Once
	started chan<- struct{}
	release <-chan struct{}
}

func (b *blockedBody) Read([]byte) (int, error) {
	b.once.Do(func() { b.started <- struct{}{} })
	<-b.release
	return 0, io.EOF
}

func TestHandlerBoundsSimultaneousTransfers(t *testing.T) {
	handler, _, key, _ := handlerFixture(t)
	handler.(*cacheHandler).transferLimit = make(chan struct{}, 2)
	token := signFixture(t, key, "key1", nil)
	_, created := rpcJSON(t, handler, "CreateCacheEntry", token, map[string]any{"key": "busy", "version": "v1"})
	upload := created["signed_upload_url"].(string)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request := httptest.NewRequest(http.MethodPut, upload, &blockedBody{started: started, release: release})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			wg.Wait()
			t.Fatal("transfer did not start")
		}
	}
	response := requestHandler(handler, http.MethodPut, upload, "", []byte("third"), "application/octet-stream")
	close(release)
	wg.Wait()
	if response.Code != 503 {
		t.Fatalf("unbounded transfer accepted: %d", response.Code)
	}
}
