package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	collectorv1 "github.com/grafana/alloy-remote-config/api/gen/proto/go/collector/v1"
	"github.com/grafana/alloy-remote-config/api/gen/proto/go/collector/v1/collectorv1connect"
	"google.golang.org/protobuf/proto"
)

type relayCache struct {
	Version     int       `json:"version"`
	Key         string    `json:"key"`
	Content     string    `json:"content"`
	Hash        string    `json:"hash"`
	Digest      string    `json:"digest"`
	ValidatedAt time.Time `json:"validated_at"`
}

// Calls for the same identity are serialized, including cache writes and reports.
type relayIdentity struct {
	mu           sync.Mutex
	registration *collectorv1.RegisterCollectorRequest
	status       *collectorv1.RemoteConfigStatus
	statusHash   string
	effective    *collectorv1.EffectiveConfig
}

type Relay struct {
	name        string
	client      collectorv1connect.CollectorServiceClient
	upstream    string
	cacheDir    string
	timeout     time.Duration
	maxAge      time.Duration
	token       string
	log         *slog.Logger
	mu          sync.Mutex
	identities  map[string]*relayIdentity
	lastSuccess time.Time
	lastError   string
	fallbacks   uint64
}

func NewRelay(upstream, cacheDir string, timeout, maxAge time.Duration, token string, log *slog.Logger) (*Relay, error) {
	hostname, _ := os.Hostname()
	name := env("RELAY_NAME", hostname)
	if name == "" {
		name = "relay"
	}
	if len(name) > 128 {
		return nil, errors.New("RELAY_NAME must be at most 128 characters")
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
			return nil, errors.New("RELAY_NAME must contain only letters, digits, dots, underscores or hyphens")
		}
	}
	u, err := url.Parse(upstream)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("UPSTREAM_URL must be an HTTP(S) base URL without credentials, query or fragment")
	}
	if timeout <= 0 || maxAge < 0 {
		return nil, errors.New("RELAY_TIMEOUT must be positive and RELAY_CACHE_MAX_AGE must be non-negative")
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("create relay cache: %w", err)
	}
	// Fail at startup rather than promise persistence on an unwritable volume.
	f, err := os.CreateTemp(cacheDir, ".probe-*")
	if err != nil {
		return nil, fmt.Errorf("relay cache is not writable: %w", err)
	}
	f.Close()
	os.Remove(f.Name())
	upstream = strings.TrimRight(upstream, "/")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Never forward configuration reports or credentials through a redirect.
	httpClient := &http.Client{Timeout: timeout, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Relay{name: name, client: collectorv1connect.NewCollectorServiceClient(httpClient, upstream, connect.WithReadMaxBytes(16<<20)), upstream: upstream, cacheDir: cacheDir, timeout: timeout, maxAge: maxAge, token: token, log: log, identities: map[string]*relayIdentity{}}, nil
}

func (r *Relay) identity(id string, local, legacy map[string]string) (string, *relayIdentity) {
	b, _ := json.Marshal(struct {
		Upstream, ID, CredentialDigest string
		Attributes                     map[string]string
	}{r.upstream, id, sha(r.token), effectiveAttrs(id, local, legacy)})
	key := sha(string(b))
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.identities[key]
	if state == nil {
		state = &relayIdentity{}
		r.identities[key] = state
	}
	return key, state
}

func relayHops(header http.Header) (int, error) {
	if header.Get("X-Alloy-Relay-Hops") == "" {
		return 0, nil
	}
	hops, err := strconv.Atoi(header.Get("X-Alloy-Relay-Hops"))
	if err != nil || hops < 0 {
		return 0, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid relay hop count"))
	}
	if hops >= 8 {
		return 0, connect.NewError(connect.CodeFailedPrecondition, errors.New("relay hop limit exceeded; check UPSTREAM_URL for a loop"))
	}
	return hops, nil
}

func relayRequest[T any](r *Relay, msg *T, hops int) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set("X-Alloy-Relay-Hops", strconv.Itoa(hops+1))
	req.Header().Set("X-Alloy-Relay-Name", r.name)
	if r.token != "" {
		req.Header().Set("Authorization", "Bearer "+r.token)
	}
	return req
}

func transientRelayError(err error) bool {
	switch connect.CodeOf(err) {
	case connect.CodeUnavailable, connect.CodeDeadlineExceeded, connect.CodeResourceExhausted, connect.CodeInternal, connect.CodeUnknown:
		return true
	}
	return false
}

func (r *Relay) record(err error, fallback bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		r.lastSuccess = time.Now()
		r.lastError = ""
	} else {
		r.lastError = connect.CodeOf(err).String()
	}
	if fallback {
		r.fallbacks++
	}
}

func (r *Relay) readCache(key string) (*relayCache, error) {
	b, err := os.ReadFile(filepath.Join(r.cacheDir, key+".json"))
	if err != nil {
		return nil, err
	}
	var cache relayCache
	if err := json.Unmarshal(b, &cache); err != nil {
		return nil, err
	}
	if cache.Version != 1 || cache.Key != key || cache.Hash == "" || cache.Digest != sha(cache.Content) || cache.ValidatedAt.IsZero() {
		return nil, errors.New("invalid relay cache")
	}
	return &cache, nil
}

func (r *Relay) writeCache(cache *relayCache) error {
	b, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(r.cacheDir, ".cache-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(r.cacheDir, cache.Key+".json"))
}

func (r *Relay) GetConfig(ctx context.Context, req *connect.Request[collectorv1.GetConfigRequest]) (*connect.Response[collectorv1.GetConfigResponse], error) {
	hops, hopErr := relayHops(req.Header())
	if hopErr != nil {
		return nil, hopErr
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errMissingID)
	}
	key, state := r.identity(req.Msg.Id, req.Msg.LocalAttributes, req.Msg.Attributes)
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	msg := proto.Clone(req.Msg).(*collectorv1.GetConfigRequest)
	if msg.RemoteConfigStatus != nil && msg.RemoteConfigStatus.Status != collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_UNSET {
		state.status = proto.Clone(msg.RemoteConfigStatus).(*collectorv1.RemoteConfigStatus)
		state.statusHash = msg.Hash
	}
	if msg.EffectiveConfig != nil {
		state.effective = proto.Clone(msg.EffectiveConfig).(*collectorv1.EffectiveConfig)
	}
	if state.statusHash == msg.Hash {
		msg.RemoteConfigStatus = state.status
	}
	msg.EffectiveConfig = state.effective
	upCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	if state.registration != nil {
		if _, err := r.client.RegisterCollector(upCtx, relayRequest(r, state.registration, hops)); err == nil {
			state.registration = nil
		}
	}
	response, err := r.client.GetConfig(upCtx, relayRequest(r, msg, hops))
	cache, _ := r.readCache(key)
	if err == nil && response.Msg.NotModified && (cache == nil || cache.Hash != response.Msg.Hash) {
		// A cold relay must fetch the full body even when Alloy already has it.
		full := proto.Clone(msg).(*collectorv1.GetConfigRequest)
		full.Hash, full.RemoteConfigStatus, full.EffectiveConfig = "", nil, nil
		response, err = r.client.GetConfig(upCtx, relayRequest(r, full, hops))
		if err == nil && response.Msg.NotModified {
			err = connect.NewError(connect.CodeUnavailable, errors.New("upstream did not return a full config for cache hydration"))
		}
	}
	if err == nil && response.Msg.Hash == "" {
		err = connect.NewError(connect.CodeUnavailable, errors.New("upstream returned an empty config hash"))
	}
	if err == nil {
		if !response.Msg.NotModified {
			cache = &relayCache{Version: 1, Key: key, Content: response.Msg.Content, Hash: response.Msg.Hash, Digest: sha(response.Msg.Content)}
		}
		cache.ValidatedAt = time.Now()
		if writeErr := r.writeCache(cache); writeErr != nil {
			r.log.Error("relay cache write failed", "id", msg.Id, "err", writeErr)
		}
		state.status, state.effective = nil, nil
		r.record(nil, false)
		return relayConfigResponse(cache, req.Msg.Hash, "upstream"), nil
	}
	r.record(err, false)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !transientRelayError(err) {
		// An explicit denial invalidates offline access for this identity too.
		if code := connect.CodeOf(err); code == connect.CodePermissionDenied || code == connect.CodeUnauthenticated || code == connect.CodeNotFound {
			if removeErr := os.Remove(filepath.Join(r.cacheDir, key+".json")); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				r.log.Error("cannot invalidate denied relay cache", "id", msg.Id, "err", removeErr)
			}
		}
		return nil, err
	}
	if cache == nil || (r.maxAge > 0 && time.Since(cache.ValidatedAt) > r.maxAge) {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("upstream unavailable and no usable cached config for this collector and attributes"))
	}
	r.record(err, true)
	r.log.Warn("relay serving cached config", "id", msg.Id, "validated_at", cache.ValidatedAt, "upstream_code", connect.CodeOf(err).String())
	return relayConfigResponse(cache, req.Msg.Hash, "cache"), nil
}

func relayConfigResponse(cache *relayCache, clientHash, source string) *connect.Response[collectorv1.GetConfigResponse] {
	msg := &collectorv1.GetConfigResponse{Hash: cache.Hash, NotModified: cache.Hash == clientHash}
	if !msg.NotModified {
		msg.Content = cache.Content
	}
	response := connect.NewResponse(msg)
	response.Header().Set("X-Config-Source", source)
	return response
}

func (r *Relay) RegisterCollector(ctx context.Context, req *connect.Request[collectorv1.RegisterCollectorRequest]) (*connect.Response[collectorv1.RegisterCollectorResponse], error) {
	hops, hopErr := relayHops(req.Header())
	if hopErr != nil {
		return nil, hopErr
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errMissingID)
	}
	_, state := r.identity(req.Msg.Id, req.Msg.LocalAttributes, req.Msg.Attributes)
	state.mu.Lock()
	defer state.mu.Unlock()
	state.registration = proto.Clone(req.Msg).(*collectorv1.RegisterCollectorRequest)
	upCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	response, err := r.client.RegisterCollector(upCtx, relayRequest(r, state.registration, hops))
	if err == nil {
		state.registration = nil
		return response, nil
	}
	if transientRelayError(err) && ctx.Err() == nil {
		r.log.Warn("relay registration deferred until next poll", "id", req.Msg.Id)
		return connect.NewResponse(&collectorv1.RegisterCollectorResponse{}), nil
	}
	return nil, err
}

func (r *Relay) UnregisterCollector(ctx context.Context, req *connect.Request[collectorv1.UnregisterCollectorRequest]) (*connect.Response[collectorv1.UnregisterCollectorResponse], error) {
	hops, hopErr := relayHops(req.Header())
	if hopErr != nil {
		return nil, hopErr
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errMissingID)
	}
	upCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	// No delayed unregister: it could incorrectly unregister a restarted collector.
	return r.client.UnregisterCollector(upCtx, relayRequest(r, req.Msg, hops))
}

func (r *Relay) registerUI(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, req *http.Request) { w.Write([]byte("ok")) })
	status := func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		writeJSON(w, map[string]any{"mode": "relay", "name": r.name, "last_upstream_success": r.lastSuccess, "upstream_error_code": r.lastError, "cache_fallbacks": r.fallbacks})
	}
	mux.HandleFunc("GET /{$}", status)
	mux.HandleFunc("GET /api/relay", status)
}
