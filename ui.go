package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
)

//go:embed web/index.html
var indexHTML []byte

//go:embed web/icon.svg
var appIcon []byte

func (s *Server) registerUI(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})

	mux.HandleFunc("GET /icon.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Write(appIcon)
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		collectors, events := s.snapshot()
		configs := s.store.Configs()
		activeCollectors, connectedRelays := s.activityCounts()
		writeJSON(w, map[string]any{
			"configs": configs,
			"summary": map[string]int{
				"active_collectors": activeCollectors,
				"config_files":      len(configs),
				"connected_relays":  connectedRelays,
			},
			"collectors": collectors,
			"events":     events,
			"revision":   readConfigRevision(env("CONFIG_REVISION_DIR", s.store.dir)),
		})
	})

	// GET /api/config/{name} — текст конфига. Имя — метка целиком, напр. "env_prod".
	mux.HandleFunc("GET /api/config/{name}", func(w http.ResponseWriter, r *http.Request) {
		c := s.store.Get(r.PathValue("name"))
		if c == nil {
			http.NotFound(w, r)
			return
		}
		if c.Error != "" {
			http.Error(w, c.Error, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(c.content))
	})

	mux.HandleFunc("GET /api/collector/{id}/config", func(w http.ResponseWriter, r *http.Request) {
		content, parts, ok := s.collectorConfigView(r.PathValue("id"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("format") == "json" {
			writeJSON(w, map[string]any{"content": content, "parts": parts})
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte(content))
	})

	mux.HandleFunc("GET /api/collector/{id}/effective-config", func(w http.ResponseWriter, r *http.Request) {
		files, receivedAt, ok := s.collectorEffectiveConfig(r.PathValue("id"))
		if !ok {
			http.Error(w, "Alloy has not reported its effective configuration yet", http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"files": files, "received_at": receivedAt})
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}
