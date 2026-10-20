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

// Binding — один лейбл на шаблон, хранится в DATA_DIR отдельно от файлов.
type Binding struct {
	Description string `json:"description,omitempty"`
	Key         string `json:"key"`
	Value       string `json:"value"` // "*" = любое значение ключа у Alloy
}

type bindingsFile struct {
	Templates map[string]Binding `json:"templates"`
}

// Config — один шаблон: файл *.alloy + опциональный лейбл.
type Config struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	LabelKey    string    `json:"label_key,omitempty"`
	LabelValue  string    `json:"label_value,omitempty"`
	Bound       bool      `json:"bound"` // есть привязка; без неё шаблон никому не отдаётся
	Hash        string    `json:"hash"`
	Size        int       `json:"size"`
	Modified    time.Time `json:"modified"`
	Error       string    `json:"error,omitempty"`
	Warning     string    `json:"warning,omitempty"`
	content     string
}

// Matches: шаблон подходит, если у Alloy есть атрибут label_key со значением
// label_value (или "*" = любое значение этого ключа).
func (c *Config) Matches(attrs map[string]string) bool {
	if c.Error != "" || !c.Bound || c.LabelKey == "" {
		return false
	}
	got, ok := attrs[c.LabelKey]
	if !ok {
		return false
	}
	return c.LabelValue == "*" || c.LabelValue == got
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
	var wrap struct {
		Templates map[string]json.RawMessage `json:"templates"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		s.log.Error("cannot parse bindings", "file", s.dataFile, "err", err)
		return
	}
	out := map[string]Binding{}
	migrated := false
	for name, chunk := range wrap.Templates {
		b, fromOld, err := parseBinding(chunk)
		if err != nil || b.Key == "" {
			continue
		}
		out[name] = b
		if fromOld {
			migrated = true
		}
	}
	s.binds = out
	s.log.Info("bindings loaded", "file", s.dataFile, "templates", len(s.binds))
	if migrated {
		if err := s.saveBindingsLocked(); err != nil {
			s.log.Error("cannot rewrite migrated bindings", "err", err)
		} else {
			s.log.Info("bindings rewritten to single-label format")
		}
	}
}

// parseBinding принимает {"key","value"} или старый {"labels":{"k":"v"}}.
func parseBinding(raw []byte) (Binding, bool, error) {
	var b Binding
	if err := json.Unmarshal(raw, &b); err != nil {
		return Binding{}, false, err
	}
	if b.Key != "" {
		if b.Value == "" {
			b.Value = "*"
		}
		return b, false, nil
	}
	var old struct {
		Description string          `json:"description"`
		Labels      json.RawMessage `json:"labels"`
	}
	if err := json.Unmarshal(raw, &old); err != nil || len(old.Labels) == 0 {
		return Binding{}, false, nil
	}
	var asMap map[string]string
	if err := json.Unmarshal(old.Labels, &asMap); err == nil {
		for k, v := range asMap {
			return Binding{Description: old.Description, Key: k, Value: v}, true, nil
		}
	}
	return Binding{}, false, nil
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

// SetBinding сохраняет один лейбл шаблона на диск и сбрасывает кэш.
func (s *Store) SetBinding(name string, b Binding) error {
	b.Key = strings.TrimSpace(b.Key)
	b.Value = strings.TrimSpace(b.Value)
	if b.Key == "" || b.Value == "" {
		return fmt.Errorf("key and value are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.binds == nil {
		s.binds = map[string]Binding{}
	}
	s.binds[name] = b
	s.cfgs = nil
	if err := s.saveBindingsLocked(); err != nil {
		return err
	}
	s.log.Info("binding updated", "template", name, "label", b.Key+"="+b.Value)
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

// Select возвращает шаблоны, чей лейбл совпал с атрибутами Alloy.
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
		if b, ok := s.binds[tmpl]; ok && b.Key != "" {
			c.Bound = true
			c.LabelKey = b.Key
			c.LabelValue = b.Value
			c.Description = b.Description
		}
		if c.Error != "" {
			if g, ok := s.good[tmpl]; ok {
				stale := *g
				stale.Warning = c.Error
				stale.Bound = c.Bound
				stale.LabelKey = c.LabelKey
				stale.LabelValue = c.LabelValue
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
	c := &Config{Name: name}
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
