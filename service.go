package main

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"log/slog"

	"connectrpc.com/connect"
	collectorv1 "github.com/grafana/alloy-remote-config/api/gen/proto/go/collector/v1"
)

// Collector — то, что мы знаем про конкретный Alloy. Хранится только в памяти.
type Collector struct {
	ID                  string            `json:"id"`
	Name                string            `json:"name,omitempty"`
	Attributes          map[string]string `json:"attributes"`
	RemoteAddr          string            `json:"remote_addr"`
	Registered          bool              `json:"registered"`
	FirstSeen           time.Time         `json:"first_seen"`
	LastSeen            time.Time         `json:"last_seen"`
	LastSynced          time.Time         `json:"last_synced,omitempty"` // подтверждение получения текущей версии
	ReceivedHash        string            `json:"received_hash,omitempty"`
	AppliedHash         string            `json:"applied_hash,omitempty"`
	EffectiveKnown      bool              `json:"effective_known"`
	EffectiveReceivedAt time.Time         `json:"effective_received_at,omitempty"`
	Polls               int               `json:"polls"`
	ClientHash          string            `json:"client_hash"` // что сейчас у Alloy
	ServerHash          string            `json:"server_hash"` // что мы ему отдаём
	Configs             []string          `json:"configs"`
	Status              string            `json:"status,omitempty"`
	StatusErr           string            `json:"status_error,omitempty"`
	StatusHash          string            `json:"status_hash,omitempty"`
	configContent       string
	configParts         []ConfigPart
	effectiveConfig     []EffectiveConfigFile
	disconnected        bool
}

// Config bodies are served on demand, never included in polling snapshots.
type EffectiveConfigFile struct {
	Name        string `json:"name"`
	Content     string `json:"content"`
	ContentType string `json:"content_type,omitempty"`
}

type Event struct {
	Time        time.Time `json:"time"`
	Type        string    `json:"type"`
	Collector   string    `json:"collector,omitempty"`
	Message     string    `json:"message"`
	Relay       string    `json:"relay,omitempty"`
	RelayAddr   string    `json:"relay_addr,omitempty"`
	RelayHops   int       `json:"relay_hops,omitempty"`
	Hash        string    `json:"hash,omitempty"`
	Bytes       int       `json:"bytes,omitempty"`
	NotModified bool      `json:"not_modified,omitempty"`
}

type Server struct {
	store *Store
	log   *slog.Logger
	// logIdle=false — не писать в лог polling'и без изменений (not_modified).
	logIdle bool

	mu         sync.Mutex
	collectors map[string]*Collector
	events     []Event // кольцевой буфер, новые в конце
	relays     map[string]time.Time
}

const maxEvents = 300

func NewServer(store *Store, log *slog.Logger, logIdle bool) *Server {
	return &Server{store: store, log: log, logIdle: logIdle, collectors: map[string]*Collector{}, relays: map[string]time.Time{}}
}

// Activity is based on recent requests, not on persistent TCP connections.
func (s *Server) activityCounts() (collectors, relays int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-5 * time.Minute)
	for _, c := range s.collectors {
		if !c.disconnected && c.LastSeen.After(cutoff) {
			collectors++
		}
	}
	for key, seen := range s.relays {
		if seen.After(cutoff) {
			relays++
		} else {
			delete(s.relays, key)
		}
	}
	return
}

func (s *Server) addEvent(typ, id, msg string) { // вызывать под s.mu
	s.events = append(s.events, Event{Time: time.Now(), Type: typ, Collector: id, Message: msg})
	if len(s.events) > maxEvents {
		s.events = s.events[len(s.events)-maxEvents:]
	}
}

func (s *Server) get(id string) *Collector { // под s.mu
	c, ok := s.collectors[id]
	if !ok {
		c = &Collector{ID: id, FirstSeen: time.Now(), Attributes: map[string]string{}, Configs: []string{}}
		s.collectors[id] = c
	}
	return c
}

func (s *Server) collectorConfig(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.collectors[id]
	if !ok || c.Polls == 0 {
		return "", false
	}
	return c.configContent, true
}

func (s *Server) collectorConfigView(id string) (string, []ConfigPart, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.collectors[id]
	if !ok || c.Polls == 0 {
		return "", nil, false
	}
	return c.configContent, c.configParts, true
}

func (s *Server) collectorEffectiveConfig(id string) ([]EffectiveConfigFile, time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.collectors[id]
	if !ok || !c.EffectiveKnown {
		return nil, time.Time{}, false
	}
	return append([]EffectiveConfigFile{}, c.effectiveConfig...), c.EffectiveReceivedAt, true
}

func effectiveAttrs(id string, local, deprecated map[string]string) map[string]string {
	src := local
	if len(src) == 0 {
		src = deprecated
	}
	out := make(map[string]string, len(src)+1)
	for k, v := range src {
		out[k] = v
	}
	// Виртуальный атрибут: файл "id_<collector id>.alloy" привязывает
	// конфиг к конкретному Alloy.
	if _, ok := out["id"]; !ok {
		out["id"] = id
	}
	return out
}

func remoteAddr(h http.Header, peer string) string {
	if xff := h.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	if i := strings.LastIndex(peer, ":"); i > 0 {
		return peer[:i]
	}
	return peer
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func statusName(st *collectorv1.RemoteConfigStatus) (string, string) {
	if st == nil || st.Status == collectorv1.RemoteConfigStatuses_RemoteConfigStatuses_UNSET {
		return "", ""
	}
	return strings.ToLower(strings.TrimPrefix(st.Status.String(), "RemoteConfigStatuses_")), st.ErrorMessage
}

func (s *Server) GetConfig(ctx context.Context, req *connect.Request[collectorv1.GetConfigRequest]) (*connect.Response[collectorv1.GetConfigResponse], error) {
	m := req.Msg
	if m.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errMissingID)
	}
	attrs := effectiveAttrs(m.Id, m.LocalAttributes, m.Attributes)
	cfgs := s.store.Select(attrs)
	content, hash, parts := mergeConfigParts(cfgs, attrs)
	names := make([]string, len(cfgs))
	for i, c := range cfgs {
		names[i] = c.Name
	}
	addr := remoteAddr(req.Header(), req.Peer().Addr)
	status, statusErr := statusName(m.RemoteConfigStatus)
	relayHops, _ := strconv.Atoi(req.Header().Get("X-Alloy-Relay-Hops"))
	if relayHops < 1 || relayHops > 8 {
		relayHops = 0
	}
	relayName := ""
	relayAddr := remoteAddr(nil, req.Peer().Addr)
	if relayHops > 0 {
		relayName = strings.TrimSpace(req.Header().Get("X-Alloy-Relay-Name"))
		if len(relayName) > 128 {
			relayName = relayName[:128]
		}
		if relayName == "" {
			relayName = relayAddr
		}
	}

	s.mu.Lock()
	c := s.get(m.Id)
	prevStatus := c.Status
	previousClientHash, previousServerHash := c.ClientHash, c.ServerHash
	c.Polls++
	c.LastSeen = time.Now()
	c.disconnected = false
	if relayHops > 0 {
		s.relays[relayName+"\x00"+relayAddr] = c.LastSeen
	}
	c.RemoteAddr = addr
	c.Attributes = attrs
	c.ClientHash = m.Hash
	c.ServerHash = hash
	c.Configs = names
	c.configContent = content
	c.configParts = parts
	if status != "" {
		c.Status, c.StatusErr = status, statusErr
		c.StatusHash = m.Hash
	} else if c.StatusHash != m.Hash {
		// A status retained for an older version must not describe a new one.
		c.Status, c.StatusErr, c.StatusHash = "", "", ""
	}
	// Omitted reports mean unchanged, while an explicitly empty report clears it.
	if m.EffectiveConfig != nil {
		files := []EffectiveConfigFile{}
		if configMap := m.EffectiveConfig.ConfigMap; configMap != nil {
			for name, file := range configMap.ConfigMap {
				if file != nil {
					files = append(files, EffectiveConfigFile{Name: name, Content: string(file.Body), ContentType: file.ContentType})
				}
			}
		}
		sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
		c.effectiveConfig, c.EffectiveKnown, c.EffectiveReceivedAt = files, true, c.LastSeen
	}
	// Alloy can report a changed effective config without resending APPLIED.
	// Only a fresh, exact single-file report proves that this version is running.
	effectiveApplied := m.EffectiveConfig != nil && len(c.effectiveConfig) == 1 &&
		c.effectiveConfig[0].Content == content && status != "failed" && status != "applying"
	if effectiveApplied {
		c.Status, c.StatusErr, c.StatusHash = "applied", "", hash
	}
	appliedHash := m.Hash
	if effectiveApplied {
		appliedHash = hash
	}
	applied := (status == "applied" || effectiveApplied) && appliedHash != "" &&
		(c.AppliedHash != appliedHash || prevStatus != "applied")
	if applied {
		c.AppliedHash = appliedHash
	}

	notModified := m.Hash == hash
	// Hash acknowledgement confirms receipt, independently of application status.
	// FAILED/APPLYING must not be presented as successful synchronization.
	if (notModified && c.Status != "failed" && c.Status != "applying") || effectiveApplied {
		if c.ReceivedHash != hash || applied {
			c.LastSynced = c.LastSeen
			if !applied {
				s.addEvent("received", m.Id, "Alloy acknowledged configuration "+short(hash))
			}
		}
		c.ReceivedHash = hash
	}
	responseKind, responseBytes := "config", len(content)
	if notModified {
		responseKind, responseBytes = "not_modified", 0
	}
	result := "not_modified"
	switch {
	case !notModified && m.Hash == "" && (c.Polls == 1 || previousServerHash != hash):
		result = "initial"
	case !notModified && (previousClientHash != m.Hash || previousServerHash != hash):
		result = "update_sent"
	case !notModified:
		result = "update_pending"
	case applied:
		result = "synced"
	}
	statusChanged := status != "" && status != prevStatus

	switch result {
	case "initial":
		s.addEvent("initial", m.Id, "first config served: "+strings.Join(names, ", ")+" ("+short(hash)+")")
	case "update_sent":
		s.addEvent("update_sent", m.Id, short(m.Hash)+" → "+short(hash)+" ["+strings.Join(names, ", ")+"]")
	}
	if applied {
		s.addEvent("synced", m.Id, "Alloy confirmed applied configuration "+short(appliedHash))
	}
	if statusChanged {
		s.addEvent("status", m.Id, status+" "+statusErr)
	}
	if relayHops > 0 {
		s.addEvent("relay_config", m.Id, fmt.Sprintf("relay %s (%s, %d hops): %s; files=[%s]; hash=%s; bytes=%d", relayName, relayAddr, relayHops, responseKind, strings.Join(names, ", "), hash, responseBytes))
		event := &s.events[len(s.events)-1]
		event.Relay, event.RelayAddr, event.RelayHops = relayName, relayAddr, relayHops
		event.Hash, event.Bytes, event.NotModified = hash, responseBytes, notModified
	}
	s.mu.Unlock()

	args := []any{
		"id", m.Id, "addr", addr, "result", result,
		"client_hash", short(m.Hash), "server_hash", short(hash),
		"configs", names, "attrs", attrs,
	}
	if status != "" {
		args = append(args, "alloy_status", status)
	}
	if relayHops > 0 {
		args = append(args, "relay", relayName, "relay_addr", relayAddr, "relay_hops", relayHops, "response", responseKind, "response_hash", hash, "bytes", responseBytes)
	}
	switch {
	case relayHops > 0:
		if status == "failed" {
			s.log.Warn("GetConfig via relay: alloy failed to apply config", append(args, "error", statusErr)...)
		} else {
			s.log.Info("GetConfig via relay", args...)
		}
	case status == "failed":
		s.log.Warn("GetConfig: alloy failed to apply config", append(args, "error", statusErr)...)
	case result == "not_modified" && !statusChanged:
		if s.logIdle {
			s.log.Info("GetConfig", args...)
		}
	default:
		s.log.Info("GetConfig", args...)
	}
	if len(cfgs) == 0 && result != "not_modified" {
		s.log.Warn("no config matched this alloy", "id", m.Id, "attrs", attrs)
	}

	resp := &collectorv1.GetConfigResponse{Hash: hash, NotModified: notModified}
	if !notModified {
		resp.Content = content
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) RegisterCollector(ctx context.Context, req *connect.Request[collectorv1.RegisterCollectorRequest]) (*connect.Response[collectorv1.RegisterCollectorResponse], error) {
	m := req.Msg
	if m.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errMissingID)
	}
	attrs := effectiveAttrs(m.Id, m.LocalAttributes, m.Attributes)
	addr := remoteAddr(req.Header(), req.Peer().Addr)

	s.mu.Lock()
	c := s.get(m.Id)
	c.Name, c.Registered, c.RemoteAddr, c.Attributes, c.LastSeen = m.Name, true, addr, attrs, time.Now()
	c.disconnected = false
	s.addEvent("register", m.Id, "registered as "+m.Name)
	s.mu.Unlock()

	s.log.Info("RegisterCollector", "id", m.Id, "name", m.Name, "addr", addr, "attrs", attrs)
	return connect.NewResponse(&collectorv1.RegisterCollectorResponse{}), nil
}

func (s *Server) UnregisterCollector(ctx context.Context, req *connect.Request[collectorv1.UnregisterCollectorRequest]) (*connect.Response[collectorv1.UnregisterCollectorResponse], error) {
	id := req.Msg.Id
	s.mu.Lock()
	if c, ok := s.collectors[id]; ok {
		c.Registered = false
		c.disconnected = true
	}
	s.addEvent("unregister", id, "unregistered (alloy shutting down)")
	s.mu.Unlock()

	s.log.Info("UnregisterCollector", "id", id, "addr", remoteAddr(req.Header(), req.Peer().Addr))
	return connect.NewResponse(&collectorv1.UnregisterCollectorResponse{}), nil
}

// snapshot — данные для UI.
func (s *Server) snapshot() (collectors []Collector, events []Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	collectors = make([]Collector, 0, len(s.collectors))
	for _, c := range s.collectors {
		collectors = append(collectors, *c)
	}
	sort.Slice(collectors, func(i, j int) bool { return collectors[i].ID < collectors[j].ID })
	events = make([]Event, len(s.events))
	for i, e := range s.events { // новые сверху
		events[len(s.events)-1-i] = e
	}
	return
}
