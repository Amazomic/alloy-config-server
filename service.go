package main

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"log/slog"

	"connectrpc.com/connect"
	collectorv1 "github.com/grafana/alloy-remote-config/api/gen/proto/go/collector/v1"
)

// Collector — то, что мы знаем про конкретный Alloy. Хранится только в памяти.
type Collector struct {
	ID         string            `json:"id"`
	Name       string            `json:"name,omitempty"`
	Attributes map[string]string `json:"attributes"`
	RemoteAddr string            `json:"remote_addr"`
	Registered bool              `json:"registered"`
	FirstSeen  time.Time         `json:"first_seen"`
	LastSeen   time.Time         `json:"last_seen"`
	LastSynced time.Time         `json:"last_synced,omitempty"` // когда Alloy последний раз подтвердил новый хэш
	Polls      int               `json:"polls"`
	ClientHash string            `json:"client_hash"` // что сейчас у Alloy
	ServerHash string            `json:"server_hash"` // что мы ему отдаём
	Configs    []string          `json:"configs"`
	Status     string            `json:"status,omitempty"`
	StatusErr  string            `json:"status_error,omitempty"`
}

type Event struct {
	Time      time.Time `json:"time"`
	Type      string    `json:"type"`
	Collector string    `json:"collector,omitempty"`
	Message   string    `json:"message"`
}

type Server struct {
	store *Store
	log   *slog.Logger
	// logIdle=false — не писать в лог polling'и без изменений (not_modified).
	logIdle bool

	mu         sync.Mutex
	collectors map[string]*Collector
	events     []Event // кольцевой буфер, новые в конце
}

const maxEvents = 300

func NewServer(store *Store, log *slog.Logger, logIdle bool) *Server {
	return &Server{store: store, log: log, logIdle: logIdle, collectors: map[string]*Collector{}}
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
	content, hash := Merge(cfgs)
	names := make([]string, len(cfgs))
	for i, c := range cfgs {
		names[i] = c.Name
	}
	addr := remoteAddr(req.Header(), req.Peer().Addr)
	status, statusErr := statusName(m.RemoteConfigStatus)

	s.mu.Lock()
	c := s.get(m.Id)
	first := c.Polls == 0
	prevClient := c.ClientHash
	prevStatus := c.Status
	c.Polls++
	c.LastSeen = time.Now()
	c.RemoteAddr = addr
	c.Attributes = attrs
	c.ClientHash = m.Hash
	c.ServerHash = hash
	c.Configs = names
	if status != "" {
		c.Status, c.StatusErr = status, statusErr
	}

	notModified := m.Hash == hash
	result := "not_modified"
	switch {
	case !notModified && m.Hash == "":
		result = "initial"
	case !notModified:
		result = "update_sent"
	case !first && prevClient != m.Hash:
		result = "synced" // Alloy вернулся с хэшем, равным нашему → применил конфиг
	}
	if result == "synced" || (first && notModified && m.Hash != "") {
		c.LastSynced = time.Now()
	}
	statusChanged := status != "" && status != prevStatus

	switch result {
	case "initial":
		s.addEvent("initial", m.Id, "first config served: "+strings.Join(names, ", ")+" ("+short(hash)+")")
	case "update_sent":
		s.addEvent("update_sent", m.Id, short(m.Hash)+" → "+short(hash)+" ["+strings.Join(names, ", ")+"]")
	case "synced":
		s.addEvent("synced", m.Id, "Alloy now runs "+short(hash))
	}
	if statusChanged {
		s.addEvent("status", m.Id, status+" "+statusErr)
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
	switch {
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
