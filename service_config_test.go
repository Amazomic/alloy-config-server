package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	collectorv1 "github.com/grafana/alloy-remote-config/api/gen/proto/go/collector/v1"
)

func configTestServer(t *testing.T) *Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewServer(NewStore(t.TempDir(), time.Hour, log), log, false)
}

func TestRevisionFromExplicitGitDirectory(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.Mkdir(gitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const commit = "0123456789012345678901234567890123456789"
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte(commit+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_REVISION_DIR", dir)
	server := configTestServer(t)
	mux := http.NewServeMux()
	server.registerUI(mux)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	var state struct {
		Revision ConfigRevision `json:"revision"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if !state.Revision.Available || state.Revision.Commit != commit || state.Revision.Error != "" {
		t.Fatalf("unexpected submodule revision: %#v", state.Revision)
	}
}

func TestActivityCounts(t *testing.T) {
	server := configTestServer(t)
	for _, id := range []string{"node-1", "node-2"} {
		req := connect.NewRequest(&collectorv1.GetConfigRequest{Id: id})
		req.Header().Set("X-Alloy-Relay-Hops", "1")
		req.Header().Set("X-Alloy-Relay-Name", "edge")
		if _, err := server.GetConfig(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	// Relay counts must not depend on the bounded event history or collector count.
	server.events = nil
	if collectors, relays := server.activityCounts(); collectors != 2 || relays != 1 {
		t.Fatalf("activity = (%d, %d), want (2, 1)", collectors, relays)
	}
	if _, err := server.UnregisterCollector(context.Background(), connect.NewRequest(&collectorv1.UnregisterCollectorRequest{Id: "node-1"})); err != nil {
		t.Fatal(err)
	}
	if collectors, relays := server.activityCounts(); collectors != 1 || relays != 1 {
		t.Fatalf("after disconnect = (%d, %d), want (1, 1)", collectors, relays)
	}
	server.collectors["node-2"].LastSeen = time.Now().Add(-6 * time.Minute)
	for key := range server.relays {
		server.relays[key] = time.Now().Add(-6 * time.Minute)
	}
	if collectors, relays := server.activityCounts(); collectors != 0 || relays != 0 {
		t.Fatalf("expired activity = (%d, %d), want (0, 0)", collectors, relays)
	}
	if len(server.relays) != 0 {
		t.Fatal("expired relays were not removed")
	}
	pollConfig(t, server, "", collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_UNSET, nil)
	if collectors, _ := server.activityCounts(); collectors != 1 {
		t.Fatalf("after reconnect = %d, want 1", collectors)
	}
}

func pollConfig(t *testing.T, server *Server, hash string, status collectorv1.RemoteConfigStatuses, effective *collectorv1.EffectiveConfig) string {
	t.Helper()
	resp, err := server.GetConfig(context.Background(), connect.NewRequest(&collectorv1.GetConfigRequest{
		Id: "node-1", Hash: hash, RemoteConfigStatus: &collectorv1.RemoteConfigStatus{Status: status}, EffectiveConfig: effective,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.Hash
}

func TestAppliedHashRequiresApplicationEvidence(t *testing.T) {
	server := configTestServer(t)
	hash := pollConfig(t, server, "", collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_UNSET, nil)
	for _, status := range []collectorv1.RemoteConfigStatuses{
		collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_UNSET, collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_APPLYING, collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_FAILED,
	} {
		pollConfig(t, server, hash, status, nil)
		cols, events := server.snapshot()
		if cols[0].AppliedHash != "" {
			t.Fatalf("status %s incorrectly confirmed application", status)
		}
		for _, event := range events {
			if event.Type == "synced" {
				t.Fatal("hash equality produced a synced event without APPLIED")
			}
		}
	}
	pollConfig(t, server, hash, collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED, nil)
	cols, _ := server.snapshot()
	confirmed := cols[0].LastSynced
	if confirmed.IsZero() || cols[0].AppliedHash != hash {
		t.Fatal("APPLIED did not confirm the configuration")
	}
	for _, status := range []collectorv1.RemoteConfigStatuses{collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_UNSET, collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED, collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_FAILED} {
		pollConfig(t, server, hash, status, nil)
		cols, _ = server.snapshot()
		if !cols[0].LastSynced.Equal(confirmed) {
			t.Fatal("unchanged or failed config moved LastSynced")
		}
	}
}

func TestEffectiveConfigReportsAndAPI(t *testing.T) {
	server := configTestServer(t)
	mux := http.NewServeMux()
	server.registerUI(mux)
	request := func(path string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		return recorder
	}
	path := "/api/collector/node-1/effective-config"
	if request(path).Code != http.StatusNotFound {
		t.Fatal("missing report must not be shown as an empty configuration")
	}
	report := &collectorv1.EffectiveConfig{ConfigMap: &collectorv1.AgentConfigMap{ConfigMap: map[string]*collectorv1.AgentConfigFile{
		"z.alloy": {Body: []byte("private reported body"), ContentType: "text/plain"},
		"a.alloy": {Body: []byte("another section")},
	}}}
	hash := pollConfig(t, server, "", collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_UNSET, report)
	files, received, known := server.collectorEffectiveConfig("node-1")
	if !known || received.IsZero() || len(files) != 2 || files[0].Name != "a.alloy" || files[1].Content != "private reported body" {
		t.Fatalf("unexpected report: %#v", files)
	}
	// Mutating the request or returned slice must not mutate stored data.
	report.ConfigMap.ConfigMap["z.alloy"].Body[0] = 'X'
	files[1].Content = "changed"
	pollConfig(t, server, hash, collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_UNSET, nil)
	response := request(path)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "private reported body") {
		t.Fatal("omitted report did not retain the previous configuration")
	}
	if strings.Contains(request("/api/state").Body.String(), "private reported body") {
		t.Fatal("effective config body leaked into polling snapshot")
	}
	sent := request("/api/collector/node-1/config")
	if strings.Contains(sent.Body.String(), "private reported body") || !strings.Contains(sent.Body.String(), "node_stack") {
		t.Fatal("server and reported configurations are not separate")
	}
	pollConfig(t, server, hash, collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_UNSET, &collectorv1.EffectiveConfig{})
	response = request(path)
	var empty struct {
		Files []EffectiveConfigFile `json:"files"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &empty); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || empty.Files == nil || len(empty.Files) != 0 {
		t.Fatal("explicit empty report must clear the previous configuration")
	}
}

func TestHashAcknowledgementWithoutStatus(t *testing.T) {
	server := configTestServer(t)
	hash := pollConfig(t, server, "", collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_UNSET, nil)
	pollConfig(t, server, hash, collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_UNSET, nil)
	cols, _ := server.snapshot()
	confirmed := cols[0].LastSynced
	if confirmed.IsZero() || cols[0].ReceivedHash != hash || cols[0].AppliedHash != "" || cols[0].Status != "" {
		t.Fatal("hash acknowledgement must confirm receipt without inventing APPLIED")
	}
	pollConfig(t, server, hash, collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_UNSET, nil)
	cols, _ = server.snapshot()
	if !cols[0].LastSynced.Equal(confirmed) {
		t.Fatal("idle polling changed synchronization time")
	}
}

func TestEffectiveConfigConfirmsApplicationWithoutStatus(t *testing.T) {
	server := configTestServer(t)
	hash := pollConfig(t, server, "", collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_UNSET, nil)
	content, _ := server.collectorConfig("node-1")
	report := &collectorv1.EffectiveConfig{ConfigMap: &collectorv1.AgentConfigMap{ConfigMap: map[string]*collectorv1.AgentConfigFile{
		"": {Body: []byte(content), ContentType: "text/plain"},
	}}}
	pollConfig(t, server, hash, collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_UNSET, report)
	cols, _ := server.snapshot()
	if cols[0].LastSynced.IsZero() || cols[0].AppliedHash != hash || cols[0].Status != "applied" {
		t.Fatal("fresh matching effective config did not confirm application")
	}
}

func TestFailedAndApplyingDoNotConfirmSynchronization(t *testing.T) {
	for _, status := range []collectorv1.RemoteConfigStatuses{
		collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_FAILED,
		collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_APPLYING,
	} {
		t.Run(status.String(), func(t *testing.T) {
			server := configTestServer(t)
			hash := pollConfig(t, server, "", collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_UNSET, nil)
			pollConfig(t, server, hash, status, nil)
			pollConfig(t, server, hash, collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_UNSET, nil)
			cols, _ := server.snapshot()
			if !cols[0].LastSynced.IsZero() || cols[0].ReceivedHash != "" || cols[0].AppliedHash != "" {
				t.Fatal("failed or in-progress configuration was marked synchronized")
			}
		})
	}
}

func TestRepeatedPendingUpdateDoesNotSpamEvents(t *testing.T) {
	server := configTestServer(t)
	for i := 0; i < 4; i++ {
		pollConfig(t, server, "older-hash", collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_UNSET, nil)
	}
	_, events := server.snapshot()
	updates := 0
	for _, event := range events {
		if event.Type == "update_sent" {
			updates++
		}
	}
	if updates != 1 {
		t.Fatalf("update_sent events = %d, want 1", updates)
	}
}

func TestConfigPartsAPIUsesDeliveredSnapshot(t *testing.T) {
	server := configTestServer(t)
	server.store.cfgs = []*Config{{Name: "env_prod", Selector: map[string]string{}, content: "// source body\n"}}
	server.store.at = time.Now()
	pollConfig(t, server, "", collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_UNSET, nil)
	// The source may change before the user opens the delivered configuration.
	server.store.cfgs[0].content = "// newer body\n"
	server.store.cfgs[0].Selector["env"] = "changed"
	mux := http.NewServeMux()
	server.registerUI(mux)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/collector/node-1/config?format=json", nil))
	var view struct {
		Content string       `json:"content"`
		Parts   []ConfigPart `json:"parts"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	var combined strings.Builder
	for _, part := range view.Parts {
		combined.WriteString(part.Content)
	}
	if recorder.Code != http.StatusOK || combined.String() != view.Content || len(view.Parts) != 3 || view.Parts[1].Name != "env_prod" {
		t.Fatal("configuration view has inconsistent parts")
	}
	if strings.Contains(view.Content, "newer body") || len(view.Parts[1].Selector) != 0 {
		t.Fatal("view metadata was rebuilt from changed source files")
	}
	raw := httptest.NewRecorder()
	mux.ServeHTTP(raw, httptest.NewRequest(http.MethodGet, "/api/collector/node-1/config", nil))
	if raw.Body.String() != view.Content || strings.Contains(raw.Body.String(), "env_prod") {
		t.Fatal("plain configuration changed or contains display metadata")
	}
}
