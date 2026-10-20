package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
)

//go:embed web/index.html
var indexHTML []byte

func (s *Server) registerUI(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		collectors, events := s.snapshot()
		writeJSON(w, map[string]any{
			"configs":    s.store.Configs(),
			"collectors": collectors,
			"events":     events,
		})
	})

	mux.HandleFunc("GET /api/config/{name}", func(w http.ResponseWriter, r *http.Request) {
		c := s.store.Get(r.PathValue("name"))
		if c == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(c.content))
	})

	// PUT /api/config/{name}/labels — повесить/обновить лейблы шаблона.
	// Body: {"labels":{"env":"prod","role":["web","api"]},"description":"..."}
	// Пустой labels {} — подходит всем Alloy. DELETE снимает привязку.
	mux.HandleFunc("PUT /api/config/{name}/labels", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if s.store.Get(name) == nil {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Description string                 `json:"description"`
			Labels      map[string]LabelValues `json:"labels"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if body.Labels == nil {
			http.Error(w, `labels required (use {} to match every Alloy)`, http.StatusBadRequest)
			return
		}
		if err := s.store.SetBinding(name, Binding{Description: body.Description, Labels: body.Labels}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, s.store.Get(name))
	})

	mux.HandleFunc("DELETE /api/config/{name}/labels", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if s.store.Get(name) == nil {
			http.NotFound(w, r)
			return
		}
		if err := s.store.DeleteBinding(name); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, s.store.Get(name))
	})

	mux.HandleFunc("GET /api/match", func(w http.ResponseWriter, r *http.Request) {
		attrs := map[string]string{}
		for k, v := range r.URL.Query() {
			if len(v) > 0 {
				attrs[k] = v[0]
			}
		}
		cfgs := s.store.Select(attrs)
		names := []string{}
		for _, c := range cfgs {
			names = append(names, c.Name)
		}
		_, hash := Merge(cfgs)
		writeJSON(w, map[string]any{"configs": names, "hash": short(hash)})
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}
