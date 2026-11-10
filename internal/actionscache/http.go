package actionscache

import (
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	cachepb "github.com/ysya/runscaler/internal/actionscache/pb/results/api/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const cacheRPCPrefix = "/twirp/github.actions.results.api.v1.CacheService/"

type HandlerConfig struct {
	Store             *Store
	Verifier          *Verifier
	BaseURL           string
	UpstreamURL       string
	UpstreamTransport http.RoundTripper
	MaxSizeBytes      int64
	MaxIdle           time.Duration
	OnCleanupError    func(error)
}

type cacheHandler struct {
	store          *Store
	verifier       *Verifier
	signer         *capabilitySigner
	proxy          *httputil.ReverseProxy
	maxBytes       int64
	maxIdle        time.Duration
	onCleanupError func(error)
	transferLimit  chan struct{}
}

func NewHandler(cfg HandlerConfig) (http.Handler, error) {
	if cfg.Store == nil || cfg.Verifier == nil {
		return nil, errors.New("cache store and verifier are required")
	}
	signer, err := newCapabilitySigner(cfg.Store.root, cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	upstream, err := url.Parse(cfg.UpstreamURL)
	if err != nil || upstream.Scheme != "https" || upstream.Hostname() == "" || upstream.User != nil || upstream.RawQuery != "" || upstream.Fragment != "" {
		return nil, errors.New("invalid Results upstream URL")
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(upstream)
			request.Out.Host = upstream.Host
			request.SetXForwarded()
		},
		Transport: cfg.UpstreamTransport,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "Results upstream unavailable", http.StatusBadGateway)
		},
	}
	return &cacheHandler{store: cfg.Store, verifier: cfg.Verifier, signer: signer, proxy: proxy,
		maxBytes: cfg.MaxSizeBytes, maxIdle: cfg.MaxIdle, onCleanupError: cfg.OnCleanupError,
		transferLimit: make(chan struct{}, 16)}, nil
}

func (h *cacheHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if strings.HasPrefix(path, "/devstoreaccount1/upload/") || strings.HasPrefix(path, "/download/") {
		select {
		case h.transferLimit <- struct{}{}:
			defer func() { <-h.transferLimit }()
		default:
			http.Error(w, "local cache transfer limit reached", http.StatusServiceUnavailable)
			return
		}
	}
	switch {
	case strings.HasPrefix(path, cacheRPCPrefix):
		h.rpc(w, r, strings.TrimPrefix(path, cacheRPCPrefix))
	case strings.HasPrefix(path, "/devstoreaccount1/upload/"):
		h.upload(w, r, strings.TrimPrefix(path, "/devstoreaccount1/upload/"))
	case strings.HasPrefix(path, "/download/"):
		h.download(w, r, strings.TrimPrefix(path, "/download/"))
	case strings.HasPrefix(path, "/twirp/github.actions.results.api.v1.CacheService"),
		strings.HasPrefix(path, "/_apis/artifactcache/"),
		strings.HasPrefix(path, "/devstoreaccount1/"),
		strings.HasPrefix(path, "/download"):
		http.Error(w, "unsupported cache operation", http.StatusNotFound)
	default:
		h.proxy.ServeHTTP(w, r)
	}
}

func (h *cacheHandler) rpc(w http.ResponseWriter, r *http.Request, method string) {
	if r.Method != http.MethodPost {
		writeTwirpError(w, http.StatusMethodNotAllowed, "bad_route", "POST required")
		return
	}
	var request proto.Message
	switch method {
	case "CreateCacheEntry":
		request = &cachepb.CreateCacheEntryRequest{}
	case "FinalizeCacheEntryUpload":
		request = &cachepb.FinalizeCacheEntryUploadRequest{}
	case "GetCacheEntryDownloadURL":
		request = &cachepb.GetCacheEntryDownloadURLRequest{}
	default:
		writeTwirpError(w, http.StatusNotFound, "bad_route", "unknown cache method")
		return
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		writeTwirpError(w, 401, "unauthenticated", "bearer token required")
		return
	}
	p, err := h.verifier.Verify(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if err != nil {
		writeTwirpError(w, 401, "unauthenticated", "invalid cache token")
		return
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (contentType != "application/json" && contentType != "application/protobuf") {
		writeTwirpError(w, 415, "bad_route", "unsupported content type")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
	if err != nil || len(body) > 1<<20 {
		writeTwirpError(w, 400, "invalid_argument", "invalid request body")
		return
	}
	if contentType == "application/json" {
		err = protojson.Unmarshal(body, request)
	} else {
		err = proto.Unmarshal(body, request)
	}
	if err != nil {
		writeTwirpError(w, 400, "invalid_argument", "invalid cache request")
		return
	}
	var response proto.Message
	switch typed := request.(type) {
	case *cachepb.CreateCacheEntryRequest:
		if err := p.AuthorizeMetadata(typed.Metadata); err != nil {
			writeTwirpError(w, 403, "permission_denied", "cache scope denied")
			return
		}
		if p.FirstWritableScope() == "" {
			writeTwirpError(w, 403, "permission_denied", "cache write denied: token is read-only")
			return
		}
		reservation, err := h.store.Reserve(r.Context(), p, typed.Key, typed.Version)
		if errors.Is(err, ErrConflict) {
			response = &cachepb.CreateCacheEntryResponse{Ok: false, Message: "cache key already exists"}
			break
		}
		if err != nil {
			writeStoreError(w, err)
			return
		}
		response = &cachepb.CreateCacheEntryResponse{Ok: true, SignedUploadUrl: h.signer.url("upload", reservation.ID, 2*time.Hour)}
	case *cachepb.FinalizeCacheEntryUploadRequest:
		if err := p.AuthorizeMetadata(typed.Metadata); err != nil {
			writeTwirpError(w, 403, "permission_denied", "cache scope denied")
			return
		}
		if p.FirstWritableScope() == "" {
			writeTwirpError(w, 403, "permission_denied", "cache write denied: token is read-only")
			return
		}
		if h.maxBytes > 0 && typed.SizeBytes > h.maxBytes {
			response = &cachepb.FinalizeCacheEntryUploadResponse{Ok: false, Message: "cache entry exceeds the local byte budget"}
			break
		}
		entry, err := h.store.Finalize(r.Context(), p, typed.Key, typed.Version, typed.SizeBytes)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if h.maxBytes > 0 {
			if err := h.store.Sweep(r.Context(), h.maxBytes, h.maxIdle); err != nil && h.onCleanupError != nil {
				h.onCleanupError(err)
			}
		}
		response = &cachepb.FinalizeCacheEntryUploadResponse{Ok: true, EntryId: entry.DatabaseID}
	case *cachepb.GetCacheEntryDownloadURLRequest:
		if err := p.AuthorizeMetadata(typed.Metadata); err != nil {
			writeTwirpError(w, 403, "permission_denied", "cache scope denied")
			return
		}
		if len(p.ReadableScopes()) == 0 {
			writeTwirpError(w, 403, "permission_denied", "cache read denied: token has no readable scope")
			return
		}
		entry, ok, err := h.store.Lookup(r.Context(), p, typed.Key, typed.RestoreKeys, typed.Version)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if ok {
			response = &cachepb.GetCacheEntryDownloadURLResponse{Ok: true, MatchedKey: entry.Key, SignedDownloadUrl: h.signer.url("download", entry.ID, time.Hour)}
		} else {
			response = &cachepb.GetCacheEntryDownloadURLResponse{Ok: false}
		}
	}
	var output []byte
	if contentType == "application/json" {
		output, err = (protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}).Marshal(response)
		w.Header().Set("Content-Type", "application/json")
	} else {
		output, err = proto.Marshal(response)
		w.Header().Set("Content-Type", "application/protobuf")
	}
	if err != nil {
		writeTwirpError(w, 500, "internal", "cannot encode response")
		return
	}
	_, _ = w.Write(output)
}

func writeTwirpError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "msg": message})
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeTwirpError(w, 404, "not_found", "cache entry not found")
	case errors.Is(err, ErrIncomplete), errors.Is(err, ErrSizeMismatch):
		writeTwirpError(w, 400, "failed_precondition", "cache upload incomplete or wrong size")
	default:
		writeTwirpError(w, 500, "internal", "local cache storage failed")
	}
}

func (h *cacheHandler) upload(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPut {
		http.Error(w, "PUT required", 405)
		return
	}
	if err := h.signer.verify("upload", id, r.URL.Query()); err != nil {
		http.Error(w, "invalid upload capability", 403)
		return
	}
	if _, err := uuid.Parse(id); err != nil {
		http.Error(w, "unknown upload", 404)
		return
	}
	var err error
	switch r.URL.Query().Get("comp") {
	case "":
		err = h.store.PutBlob(r.Context(), id, r.Body)
	case "block":
		blockID := r.URL.Query().Get("blockid")
		if blockID == "" || len(blockID) > 256 {
			http.Error(w, "invalid block ID", 400)
			return
		}
		if _, err := base64.StdEncoding.DecodeString(blockID); err != nil {
			http.Error(w, "invalid block ID", 400)
			return
		}
		err = h.store.PutPart(r.Context(), id, blockID, r.Body)
	case "blocklist":
		var blockIDs []string
		blockIDs, err = parseBlockList(io.LimitReader(r.Body, 10<<20+1))
		if err == nil {
			err = h.store.SetBlockList(r.Context(), id, blockIDs)
		}
	default:
		http.Error(w, "unsupported upload operation", 400)
		return
	}
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "upload expired", 404)
			return
		}
		if errors.Is(err, ErrStagingFull) {
			http.Error(w, "local cache staging budget exceeded", 507)
			return
		}
		if errors.Is(err, ErrIncomplete) {
			http.Error(w, "incomplete block list", 400)
			return
		}
		http.Error(w, "local upload failed", 500)
		return
	}
	w.Header().Set("x-ms-request-id", uuid.NewString())
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

func parseBlockList(body io.Reader) ([]string, error) {
	decoder := xml.NewDecoder(body)
	var blockIDs []string
	root := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch item := token.(type) {
		case xml.StartElement:
			if !root {
				if item.Name.Local != "BlockList" {
					return nil, ErrIncomplete
				}
				root = true
				continue
			}
			if item.Name.Local != "Latest" && item.Name.Local != "Committed" && item.Name.Local != "Uncommitted" {
				return nil, ErrIncomplete
			}
			var id string
			if err := decoder.DecodeElement(&id, &item); err != nil {
				return nil, err
			}
			if id == "" || len(id) > 256 {
				return nil, ErrIncomplete
			}
			if _, err := base64.StdEncoding.DecodeString(id); err != nil {
				return nil, ErrIncomplete
			}
			blockIDs = append(blockIDs, id)
			if len(blockIDs) > 50000 {
				return nil, ErrIncomplete
			}
		}
	}
	if !root || len(blockIDs) == 0 {
		return nil, ErrIncomplete
	}
	return blockIDs, nil
}

func (h *cacheHandler) download(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "GET or HEAD required", 405)
		return
	}
	if err := h.signer.verify("download", id, r.URL.Query()); err != nil {
		http.Error(w, "invalid download capability", 403)
		return
	}
	entry, err := h.store.EntryByID(r.Context(), id)
	if err != nil {
		http.Error(w, "cache object not found", 404)
		return
	}
	reader, err := h.store.OpenEntry(entry)
	if err != nil {
		http.Error(w, "cache object unavailable", 404)
		return
	}
	defer reader.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "cache", time.Time{}, reader)
}
