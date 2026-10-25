package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	collectorv1 "github.com/grafana/alloy-remote-config/api/gen/proto/go/collector/v1"
	"github.com/grafana/alloy-remote-config/api/gen/proto/go/collector/v1/collectorv1connect"
)

type relayUpstreamStub struct {
	collectorv1connect.UnimplementedCollectorServiceHandler
	get      func(context.Context, *connect.Request[collectorv1.GetConfigRequest]) (*connect.Response[collectorv1.GetConfigResponse], error)
	register func(*collectorv1.RegisterCollectorRequest) error
}

func (s *relayUpstreamStub) GetConfig(ctx context.Context, req *connect.Request[collectorv1.GetConfigRequest]) (*connect.Response[collectorv1.GetConfigResponse], error) {
	return s.get(ctx, req)
}
func (s *relayUpstreamStub) RegisterCollector(ctx context.Context, req *connect.Request[collectorv1.RegisterCollectorRequest]) (*connect.Response[collectorv1.RegisterCollectorResponse], error) {
	if s.register != nil {
		if err := s.register(req.Msg); err != nil {
			return nil, err
		}
	}
	return connect.NewResponse(&collectorv1.RegisterCollectorResponse{}), nil
}

func relayTestUpstream(t *testing.T, stub *relayUpstreamStub) string {
	t.Helper()
	mux := http.NewServeMux()
	path, handler := collectorv1connect.NewCollectorServiceHandler(stub)
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func relayForTest(t *testing.T, url, dir string, timeout, maxAge time.Duration) *Relay {
	t.Helper()
	r, err := NewRelay(url, dir, timeout, maxAge, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func relayPoll(t *testing.T, r *Relay, id, hash, env string) (*connect.Response[collectorv1.GetConfigResponse], error) {
	t.Helper()
	return r.GetConfig(context.Background(), connect.NewRequest(&collectorv1.GetConfigRequest{Id: id, Hash: hash, LocalAttributes: map[string]string{"env": env}}))
}

func TestRelayPersistentFallbackIsolationAndRecovery(t *testing.T) {
	var offline bool
	content := "remote config A"
	stub := &relayUpstreamStub{get: func(ctx context.Context, req *connect.Request[collectorv1.GetConfigRequest]) (*connect.Response[collectorv1.GetConfigResponse], error) {
		if offline {
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("offline"))
		}
		hash := sha(content)
		msg := &collectorv1.GetConfigResponse{Hash: hash, NotModified: hash == req.Msg.Hash}
		if !msg.NotModified {
			msg.Content = content
		}
		return connect.NewResponse(msg), nil
	}}
	url, dir := relayTestUpstream(t, stub), t.TempDir()
	r := relayForTest(t, url, dir, time.Second, 0)
	first, err := relayPoll(t, r, "node-1", "", "prod")
	if err != nil || first.Msg.Content != content || first.Header().Get("X-Config-Source") != "upstream" {
		t.Fatalf("initial: %v, %v", first, err)
	}
	offline = true
	// Recreate the relay to prove that fallback comes from disk, not process memory.
	r = relayForTest(t, url, dir, time.Second, 0)
	fallback, err := relayPoll(t, r, "node-1", "", "prod")
	if err != nil || fallback.Msg.Content != content || fallback.Header().Get("X-Config-Source") != "cache" {
		t.Fatalf("fallback: %v, %v", fallback, err)
	}
	unchanged, err := relayPoll(t, r, "node-1", first.Msg.Hash, "prod")
	if err != nil || !unchanged.Msg.NotModified || unchanged.Msg.Content != "" {
		t.Fatalf("unchanged: %v, %v", unchanged, err)
	}
	for _, identity := range [][2]string{{"node-2", "prod"}, {"node-1", "dev"}} {
		if _, err := relayPoll(t, r, identity[0], "", identity[1]); connect.CodeOf(err) != connect.CodeUnavailable {
			t.Fatal("cache crossed identity or attribute boundary")
		}
	}
	offline = false
	content = "remote config B"
	next, err := relayPoll(t, r, "node-1", first.Msg.Hash, "prod")
	if err != nil || next.Msg.Content != content || next.Msg.Hash == first.Msg.Hash {
		t.Fatalf("recovery: %v, %v", next, err)
	}
}

func TestRelayHydratesNotModifiedAndReplaysReports(t *testing.T) {
	var offline bool
	var registrations int
	var gotStatus bool
	var gotEffective bool
	var fullRequests int
	content, hash := "current remote config", "upstream-opaque-hash"
	stub := &relayUpstreamStub{}
	stub.register = func(req *collectorv1.RegisterCollectorRequest) error {
		if offline {
			return connect.NewError(connect.CodeUnavailable, errors.New("offline"))
		}
		if req.Name != "node name" {
			t.Error("registration name lost")
		}
		registrations++
		return nil
	}
	stub.get = func(ctx context.Context, req *connect.Request[collectorv1.GetConfigRequest]) (*connect.Response[collectorv1.GetConfigResponse], error) {
		if offline {
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("offline"))
		}
		if req.Msg.RemoteConfigStatus != nil {
			gotStatus = req.Msg.RemoteConfigStatus.Status == collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED
		}
		if req.Msg.EffectiveConfig != nil {
			gotEffective = true
		}
		if req.Msg.Hash == hash {
			return connect.NewResponse(&collectorv1.GetConfigResponse{Hash: hash, NotModified: true}), nil
		}
		fullRequests++
		return connect.NewResponse(&collectorv1.GetConfigResponse{Hash: hash, Content: content}), nil
	}
	r := relayForTest(t, relayTestUpstream(t, stub), t.TempDir(), time.Second, 0)
	resp, err := relayPoll(t, r, "node-1", hash, "prod")
	if err != nil || !resp.Msg.NotModified || fullRequests != 1 {
		t.Fatalf("cache hydration failed: %v, %v", resp, err)
	}
	offline = true
	_, err = r.RegisterCollector(context.Background(), connect.NewRequest(&collectorv1.RegisterCollectorRequest{Id: "node-1", Name: "node name", LocalAttributes: map[string]string{"env": "prod"}}))
	if err != nil {
		t.Fatalf("offline registration should not prevent startup: %v", err)
	}
	_, err = r.GetConfig(context.Background(), connect.NewRequest(&collectorv1.GetConfigRequest{
		Id: "node-1", Hash: hash, LocalAttributes: map[string]string{"env": "prod"},
		RemoteConfigStatus: &collectorv1.RemoteConfigStatus{Status: collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED},
		EffectiveConfig:    &collectorv1.EffectiveConfig{},
	}))
	if err != nil {
		t.Fatal(err)
	}
	offline = false
	if _, err = relayPoll(t, r, "node-1", hash, "prod"); err != nil {
		t.Fatal(err)
	}
	if registrations != 1 || !gotStatus || !gotEffective {
		t.Fatalf("reports not replayed: registrations=%d status=%v effective=%v", registrations, gotStatus, gotEffective)
	}
}

func TestRelayDenialExpiresAndCorruption(t *testing.T) {
	code := connect.Code(0)
	stub := &relayUpstreamStub{get: func(ctx context.Context, req *connect.Request[collectorv1.GetConfigRequest]) (*connect.Response[collectorv1.GetConfigResponse], error) {
		if code != 0 {
			return nil, connect.NewError(code, errors.New("upstream failure"))
		}
		return connect.NewResponse(&collectorv1.GetConfigResponse{Hash: "hash", Content: "body"}), nil
	}}
	r := relayForTest(t, relayTestUpstream(t, stub), t.TempDir(), time.Second, 0)
	if _, err := relayPoll(t, r, "node-1", "", "prod"); err != nil {
		t.Fatal(err)
	}
	code = connect.CodePermissionDenied
	if _, err := relayPoll(t, r, "node-1", "", "prod"); connect.CodeOf(err) != code {
		t.Fatal("denial was hidden by cache")
	}
	code = connect.CodeUnavailable
	if _, err := relayPoll(t, r, "node-1", "", "prod"); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatal("denied cache was reused")
	}
	code = 0
	if _, err := relayPoll(t, r, "node-1", "", "prod"); err != nil {
		t.Fatal(err)
	}
	key, _ := r.identity("node-1", map[string]string{"env": "prod"}, nil)
	cache, _ := r.readCache(key)
	cache.ValidatedAt = time.Now().Add(-time.Hour)
	if err := r.writeCache(cache); err != nil {
		t.Fatal(err)
	}
	r.maxAge = time.Minute
	code = connect.CodeUnavailable
	if _, err := relayPoll(t, r, "node-1", "", "prod"); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatal("expired cache was served")
	}
	r.maxAge = 0
	if err := os.WriteFile(filepath.Join(r.cacheDir, key+".json"), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := relayPoll(t, r, "node-1", "", "prod"); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatal("corrupt cache was served")
	}
}

func TestRelayTimeoutAndConcurrentWireRequests(t *testing.T) {
	stub := &relayUpstreamStub{get: func(ctx context.Context, req *connect.Request[collectorv1.GetConfigRequest]) (*connect.Response[collectorv1.GetConfigResponse], error) {
		return connect.NewResponse(&collectorv1.GetConfigResponse{Hash: "hash", Content: "body"}), nil
	}}
	r := relayForTest(t, relayTestUpstream(t, stub), t.TempDir(), 50*time.Millisecond, 0)
	mux := http.NewServeMux()
	path, handler := collectorv1connect.NewCollectorServiceHandler(r)
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()
	client := collectorv1connect.NewCollectorServiceClient(server.Client(), server.URL)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.GetConfig(context.Background(), connect.NewRequest(&collectorv1.GetConfigRequest{Id: "node-1"}))
			if err != nil || resp.Msg.Content != "body" {
				t.Errorf("wire request: %v", err)
			}
		}()
	}
	wg.Wait()
	stub.get = func(ctx context.Context, req *connect.Request[collectorv1.GetConfigRequest]) (*connect.Response[collectorv1.GetConfigResponse], error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	start := time.Now()
	resp, err := r.GetConfig(context.Background(), connect.NewRequest(&collectorv1.GetConfigRequest{Id: "node-1"}))
	if err != nil || resp.Header().Get("X-Config-Source") != "cache" || time.Since(start) > time.Second {
		t.Fatalf("timeout fallback failed: %v", err)
	}
}

func TestRelaySettingsValidation(t *testing.T) {
	for _, url := range []string{"", "ftp://server", "http://user:password@server", "http://server?token=secret"} {
		if _, err := NewRelay(url, t.TempDir(), time.Second, 0, "", slog.Default()); err == nil {
			t.Errorf("accepted invalid URL %q", url)
		}
	}
}

func TestRelayCredentialsAndHopLimit(t *testing.T) {
	t.Setenv("RELAY_NAME", "edge-prod")
	stub := &relayUpstreamStub{get: func(ctx context.Context, req *connect.Request[collectorv1.GetConfigRequest]) (*connect.Response[collectorv1.GetConfigResponse], error) {
		if req.Header().Get("Authorization") != "Bearer configured-token" || req.Header().Get("X-Alloy-Relay-Hops") != "3" || req.Header().Get("X-Alloy-Relay-Name") != "edge-prod" {
			t.Error("configured credentials or hop count not forwarded correctly")
		}
		return connect.NewResponse(&collectorv1.GetConfigResponse{Hash: "hash", Content: "body"}), nil
	}}
	r := relayForTest(t, relayTestUpstream(t, stub), t.TempDir(), time.Second, 0)
	r.token = "configured-token"
	req := connect.NewRequest(&collectorv1.GetConfigRequest{Id: "node-1"})
	req.Header().Set("Authorization", "Bearer untrusted-client-token")
	req.Header().Set("X-Alloy-Relay-Hops", "2")
	if _, err := r.GetConfig(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req.Header().Set("X-Alloy-Relay-Hops", "8")
	if _, err := r.GetConfig(context.Background(), req); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatal("hop limit not enforced")
	}
	// A different upstream or credential must never inherit an existing cache.
	oldKey, _ := r.identity("node-1", nil, nil)
	r.token = "different-token"
	newKey, _ := r.identity("node-1", nil, nil)
	if newKey == oldKey {
		t.Fatal("credentials do not isolate cache identity")
	}
	r.upstream += "/another-server"
	upstreamKey, _ := r.identity("node-1", nil, nil)
	if upstreamKey == newKey {
		t.Fatal("upstream does not isolate cache identity")
	}
}

func TestManagerLogsRelayResponsesAndEvents(t *testing.T) {
	t.Setenv("RELAY_NAME", "edge-prod")
	dir := t.TempDir()
	privateContent := "// unique private config body\nprometheus.exporter.unix \"host\" { }"
	if err := os.WriteFile(filepath.Join(dir, "env_prod.alloy"), []byte(privateContent), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	manager := NewServer(NewStore(dir, time.Hour, logger), logger, false)
	mux := http.NewServeMux()
	path, handler := collectorv1connect.NewCollectorServiceHandler(manager)
	mux.Handle(path, handler)
	upstream := httptest.NewServer(mux)
	defer upstream.Close()
	r := relayForTest(t, upstream.URL, t.TempDir(), time.Second, 0)
	first, err := relayPoll(t, r, "node-1", "", "prod")
	if err != nil {
		t.Fatal(err)
	}
	second, err := relayPoll(t, r, "node-1", first.Msg.Hash, "prod")
	if err != nil || !second.Msg.NotModified {
		t.Fatalf("unchanged response: %v", err)
	}
	_, events := manager.snapshot()
	var relayEvents []Event
	for _, event := range events {
		if event.Type == "relay_config" {
			relayEvents = append(relayEvents, event)
		}
	}
	if len(relayEvents) != 2 {
		t.Fatalf("expected both relay requests in manager events, got %d", len(relayEvents))
	}
	for i, event := range relayEvents {
		if event.Time.IsZero() || event.Relay != "edge-prod" || event.RelayAddr == "" || event.RelayHops != 1 || event.Hash != first.Msg.Hash || !strings.Contains(event.Message, "env_prod") {
			t.Fatalf("incomplete relay event: %#v", event)
		}
		// snapshot returns newest events first.
		if (i == 1 && (event.NotModified || event.Bytes != len(first.Msg.Content))) || (i == 0 && (!event.NotModified || event.Bytes != 0)) {
			t.Fatalf("incorrect response details: %#v", event)
		}
	}
	if strings.Count(logs.String(), "GetConfig via relay") != 2 || !strings.Contains(logs.String(), `"response":"not_modified"`) || !strings.Contains(logs.String(), `"relay":"edge-prod"`) {
		t.Fatalf("relay logs missing when LOG_NOT_MODIFIED=false: %s", logs.String())
	}
	if strings.Contains(logs.String(), "unique private config body") {
		t.Fatal("configuration body leaked into manager logs")
	}
	// A direct collector request must not acquire relay metadata or relay events.
	pollConfig(t, manager, first.Msg.Hash, collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_UNSET, nil)
	_, events = manager.snapshot()
	count := 0
	for _, event := range events {
		if event.Type == "relay_config" {
			count++
		}
	}
	if count != 2 {
		t.Fatal("direct request was mislabeled as relayed")
	}
}
