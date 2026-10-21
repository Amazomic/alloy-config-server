package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Config — один файл-конфиг с селектором атрибутов.
type Config struct {
	Name     string            `json:"name"`
	Label    string            `json:"label"`
	Selector map[string]string `json:"selector"`
	Key      string            `json:"key,omitempty"`
	Value    string            `json:"value,omitempty"`
	Always   bool              `json:"always"`
	Hash     string            `json:"hash"`
	Size     int               `json:"size"`
	Modified time.Time         `json:"modified"`
	Error    string            `json:"error,omitempty"`
	Warning  string            `json:"warning,omitempty"`
	content  string
}

// Matches requires every selector attribute to match; additional collector
// attributes do not prevent a match. An empty selector matches every collector.
func (c *Config) Matches(attrs map[string]string) bool {
	if c.Error != "" {
		return false
	}
	for key, value := range c.Selector {
		if got, ok := attrs[key]; !ok || got != value {
			return false
		}
	}
	return true
}

// parseName reads selectors from "<key>=<value>" or legacy "<key>_<value>" names.
func parseName(name string) (key, value string, always bool, ok bool) {
	if name == "*" {
		return "", "", true, true
	}
	if i := strings.Index(name, "="); i > 0 && i < len(name)-1 {
		return name[:i], name[i+1:], false, true
	}
	i := strings.Index(name, "_")
	if i <= 0 || i == len(name)-1 {
		return "", "", false, false
	}
	return name[:i], name[i+1:], false, true
}

type Store struct {
	dir string
	ttl time.Duration
	log *slog.Logger

	mu      sync.Mutex
	at      time.Time
	cfgs    []*Config
	lastErr map[string]string
	good    map[string]*Config
}

func NewStore(dir string, ttl time.Duration, log *slog.Logger) *Store {
	return &Store{
		dir:     dir,
		ttl:     ttl,
		log:     log,
		lastErr: map[string]string{},
		good:    map[string]*Config{},
	}
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

// Select returns configs whose selectors match the collector attributes.
// Configs with empty selectors come first, followed by name-sorted configs.
func (s *Store) Select(attrs map[string]string) []*Config {
	var out []*Config
	for _, c := range s.Configs() {
		if c.Matches(attrs) {
			out = append(out, c)
		}
	}
	return out
}

// Merge склеивает выбранные конфиги в один текст и считает хэш.
func Merge(cfgs []*Config) (content, hash string) {
	return MergeForAttributes(cfgs, nil)
}

// MergeForAttributes renders collector-specific placeholders before hashing
// the merged config so every collector receives a matching content hash.
func MergeForAttributes(cfgs []*Config, attrs map[string]string) (content, hash string) {
	var b strings.Builder
	id := attrs["id"]
	if id != "" {
		id = escapeHCLString(id)
	}
	for _, c := range cfgs {
		configContent := c.content
		if id != "" {
			configContent = strings.ReplaceAll(configContent, "{{id}}", id)
			configContent = strings.ReplaceAll(configContent, "stand.190", id)
		}
		fmt.Fprintf(&b, "// ===== %s =====\n%s\n", c.Name, configContent)
	}
	content = b.String()
	return content, sha(content)
}

func escapeHCLString(value string) string {
	return strings.NewReplacer(
		"\\", "\\\\",
		"\"", "\\\"",
		"\n", "\\n",
		"\r", "\\r",
		"\t", "\\t",
	).Replace(value)
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
		fname := e.Name()
		if strings.HasPrefix(fname, ".") || e.IsDir() || !strings.HasSuffix(fname, ".alloy") {
			continue
		}
		name := strings.TrimSuffix(fname, ".alloy")
		c := s.loadFile(name, filepath.Join(s.dir, fname))
		if c.Error != "" {
			if g, ok := s.good[name]; ok {
				stale := *g
				stale.Warning = c.Error
				c = &stale
			}
		} else {
			s.good[name] = c
		}
		seen[name] = true
		out = append(out, c)

		problem := c.Error + c.Warning
		if prev := s.lastErr[name]; prev != problem {
			s.lastErr[name] = problem
			switch {
			case c.Warning != "":
				s.log.Warn("config is broken, serving last good version", "config", name, "reason", c.Warning)
			case c.Error != "":
				s.log.Warn("config unreadable", "config", name, "reason", c.Error)
			default:
				s.log.Info("config is valid again", "config", name)
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
	sort.Slice(out, func(i, j int) bool {
		if out[i].Always != out[j].Always {
			return out[i].Always
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (s *Store) loadFile(name, path string) *Config {
	c := &Config{Name: name, Label: name}
	if key, value, always, ok := parseName(name); ok {
		if !always {
			c.Selector = map[string]string{key: value}
		} else {
			c.Selector = map[string]string{}
		}
	} else {
		c.Error = "invalid config filename: expected \"<key>=<value>.alloy\", legacy \"<key>_<value>.alloy\", or \"*.alloy\""
		return c
	}
	c.Always = len(c.Selector) == 0
	if len(c.Selector) == 1 {
		for key, value := range c.Selector {
			c.Key, c.Value = key, value
		}
	}
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
