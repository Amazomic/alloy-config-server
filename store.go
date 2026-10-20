package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// LabelValues — одно или несколько допустимых значений лейбла.
// В JSON: "prod" или ["web", "api"].
type LabelValues []string

func (l *LabelValues) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*l = LabelValues{s}
		return nil
	}
	var ss []string
	if err := json.Unmarshal(b, &ss); err != nil {
		return err
	}
	*l = ss
	return nil
}

// Binding — лейблы шаблона, хранятся отдельно от файлов (в DATA_DIR).
type Binding struct {
	Description string                 `json:"description,omitempty"`
	Labels      map[string]LabelValues `json:"labels"`
}

type bindingsFile struct {
	Templates map[string]Binding `json:"templates"`
}

// Config — один шаблон: файл *.alloy + опциональная привязка лейблов.
type Config struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Labels      map[string]LabelValues `json:"labels"`
	Bound       bool                   `json:"bound"` // есть запись в bindings; без неё шаблон никому не отдаётся
	Hash        string                 `json:"hash"`
	Size        int                    `json:"size"`
	Modified    time.Time              `json:"modified"`
	Error       string                 `json:"error,omitempty"`
	Warning     string                 `json:"warning,omitempty"`
	content     string
}

// Matches: шаблон подходит, если он bound, без ошибки, и КАЖДЫЙ его лейбл
// есть в атрибутах Alloy с одним из значений ("*" = любое).
// Пустой набор лейблов подходит всем.
func (c *Config) Matches(attrs map[string]string) bool {
	if c.Error != "" || !c.Bound {
		return false
	}
	for k, want := range c.Labels {
		got, ok := attrs[k]
		if !ok {
			return false
		}
		hit := false
		for _, w := range want {
			if w == "*" || w == got {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

type Store struct {
	dir      string
	dataFile string
	ttl      time.Duration
	log      *slog.Logger

	mu      sync.Mutex
	at      time.Time
	cfgs    []*Config
	binds   map[string]Binding
	lastErr map[string]string
	good    map[string]*Config
}

func NewStore(dir, dataDir string, ttl time.Duration, log *slog.Logger) *Store {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Error("cannot create data dir", "dir", dataDir, "err", err)
	}
	s := &Store{
		dir:      dir,
		dataFile: filepath.Join(dataDir, "bindings.json"),
		ttl:      ttl,
		log:      log,
		binds:    map[string]Binding{},
		lastErr:  map[string]string{},
		good:     map[string]*Config{},
	}
	s.loadBindings()
	return s
}

func (s *Store) loadBindings() {
	raw, err := os.ReadFile(s.dataFile)
	if err != nil {
		if !os.IsNotExist(err) {
			s.log.Error("cannot read bindings", "file", s.dataFile, "err", err)
		}
		return
	}
	var bf bindingsFile
	if err := json.Unmarshal(raw, &bf); err != nil {
		s.log.Error("cannot parse bindings", "file", s.dataFile, "err", err)
		return
	}
	if bf.Templates == nil {
		bf.Templates = map[string]Binding{}
	}
	for name, b := range bf.Templates {
		if b.Labels == nil {
			b.Labels = map[string]LabelValues{}
			bf.Templates[name] = b
		}
	}
	s.binds = bf.Templates
	s.log.Info("bindings loaded", "file", s.dataFile, "templates", len(s.binds))
}

func (s *Store) saveBindingsLocked() error {
	bf := bindingsFile{Templates: s.binds}
	if bf.Templates == nil {
		bf.Templates = map[string]Binding{}
	}
	raw, err := json.MarshalIndent(bf, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.dataFile + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.dataFile)
}

// SetBinding сохраняет лейблы шаблона на диск и сбрасывает кэш списка.
func (s *Store) SetBinding(name string, b Binding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b.Labels == nil {
		b.Labels = map[string]LabelValues{}
	}
	if s.binds == nil {
		s.binds = map[string]Binding{}
	}
	s.binds[name] = b
	s.cfgs = nil // пересканировать с новыми лейблами
	if err := s.saveBindingsLocked(); err != nil {
		return err
	}
	s.log.Info("binding updated", "template", name, "labels", b.Labels)
	return nil
}

// DeleteBinding убирает привязку — шаблон перестаёт отдаваться кому-либо.
func (s *Store) DeleteBinding(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.binds, name)
	s.cfgs = nil
	if err := s.saveBindingsLocked(); err != nil {
		return err
	}
	s.log.Info("binding removed", "template", name)
	return nil
}

func (s *Store) Configs() []*Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfgs == nil || time.Since(s.at) > s.ttl {
		s.cfgs = s.scan()
		s.at = time.Now()
	}
	return s.cfgs
}

func (s *Store) Get(name string) *Config {
	for _, c := range s.Configs() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// Select возвращает шаблоны, подходящие под атрибуты, в порядке имён.
func (s *Store) Select(attrs map[string]string) []*Config {
	var out []*Config
	for _, c := range s.Configs() {
		if c.Matches(attrs) {
			out = append(out, c)
		}
	}
	return out
}

// Merge склеивает выбранные шаблоны в один текст и считает хэш.
func Merge(cfgs []*Config) (content, hash string) {
	var b strings.Builder
	for _, c := range cfgs {
		fmt.Fprintf(&b, "// ===== %s =====\n%s\n", c.Name, c.content)
	}
	content = b.String()
	return content, sha(content)
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (s *Store) scan() []*Config {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		s.log.Error("cannot read config dir", "dir", s.dir, "err", err)
		return []*Config{}
	}
	out := []*Config{}
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || e.IsDir() {
			continue
		}
		if !strings.HasSuffix(name, ".alloy") {
			continue
		}
		tmpl := strings.TrimSuffix(name, ".alloy")
		c := s.loadFile(tmpl, filepath.Join(s.dir, name))
		if b, ok := s.binds[tmpl]; ok {
			c.Bound = true
			c.Labels = b.Labels
			if c.Labels == nil {
				c.Labels = map[string]LabelValues{}
			}
			c.Description = b.Description
		} else {
			c.Labels = map[string]LabelValues{}
		}
		if c.Error != "" {
			if g, ok := s.good[tmpl]; ok {
				stale := *g
				stale.Warning = c.Error
				stale.Bound = c.Bound
				stale.Labels = c.Labels
				stale.Description = c.Description
				c = &stale
			}
		} else {
			s.good[tmpl] = c
		}
		seen[tmpl] = true
		out = append(out, c)

		problem := c.Error + c.Warning
		if prev := s.lastErr[tmpl]; prev != problem {
			s.lastErr[tmpl] = problem
			switch {
			case c.Warning != "":
				s.log.Warn("template is broken, serving last good version", "template", tmpl, "reason", c.Warning)
			case c.Error != "":
				s.log.Warn("template unreadable", "template", tmpl, "reason", c.Error)
			default:
				s.log.Info("template is valid again", "template", tmpl)
			}
		}
	}
	for n := range s.lastErr {
		if !seen[n] {
			delete(s.lastErr, n)
		}
	}
	for n := range s.good {
		if !seen[n] {
			delete(s.good, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) loadFile(name, path string) *Config {
	c := &Config{Name: name, Labels: map[string]LabelValues{}}
	data, err := os.ReadFile(path)
	if err != nil {
		c.Error = "read: " + err.Error()
		return c
	}
	if fi, err := os.Stat(path); err == nil {
		c.Modified = fi.ModTime()
	}
	c.content = strings.TrimRight(string(data), "\n") + "\n"
	c.Size = len(c.content)
	c.Hash = sha(c.content)[:12]
	return c
}
