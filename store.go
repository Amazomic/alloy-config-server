package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// LabelValues — одно или несколько допустимых значений лейбла.
// В YAML: `env: prod` или `role: [web, api]`.
type LabelValues []string

func (l *LabelValues) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*l = LabelValues{n.Value}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: label values must be scalars", c.Line)
			}
			*l = append(*l, c.Value)
		}
	default:
		return fmt.Errorf("line %d: label value must be a string or a list of strings", n.Line)
	}
	return nil
}

type labelsFile struct {
	Description string                 `yaml:"description"`
	Labels      map[string]LabelValues `yaml:"labels"`
}

// Config — один pipeline: папка с *.alloy файлами и labels.yaml.
type Config struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Labels      map[string]LabelValues `json:"labels"`
	Files       []string               `json:"files"`
	Hash        string                 `json:"hash"`
	Size        int                    `json:"size"`
	Modified    time.Time              `json:"modified"`
	Error       string                 `json:"error,omitempty"`   // если задано — конфиг отключён
	Warning     string                 `json:"warning,omitempty"` // файлы сейчас битые, отдаётся последняя рабочая версия
	content     string
}

// Matches: конфиг подходит, если КАЖДЫЙ его лейбл присутствует в атрибутах
// Alloy с одним из перечисленных значений ("*" = любое значение).
// Пустой набор лейблов (`labels: {}`) подходит всем.
func (c *Config) Matches(attrs map[string]string) bool {
	if c.Error != "" {
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
	dir string
	ttl time.Duration
	log *slog.Logger

	mu      sync.Mutex
	at      time.Time
	cfgs    []*Config
	lastErr map[string]string  // чтобы не спамить одной и той же ошибкой
	good    map[string]*Config // последняя валидная версия каждого конфига
}

func NewStore(dir string, ttl time.Duration, log *slog.Logger) *Store {
	return &Store{dir: dir, ttl: ttl, log: log, lastErr: map[string]string{}, good: map[string]*Config{}}
}

// Configs возвращает актуальный список. Папка перечитывается не чаще раза в ttl —
// без fsnotify, потому что он ненадёжен на bind-mount'ах и сетевых ФС.
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

// Select возвращает конфиги, подходящие под атрибуты, в порядке имён папок.
func (s *Store) Select(attrs map[string]string) []*Config {
	var out []*Config
	for _, c := range s.Configs() {
		if c.Matches(attrs) {
			out = append(out, c)
		}
	}
	return out
}

// Merge склеивает выбранные конфиги в один текст (Alloy принимает ровно один) и считает хэш.
func Merge(cfgs []*Config) (content, hash string) {
	var b strings.Builder
	for _, c := range cfgs {
		fmt.Fprintf(&b, "// ===== pipeline: %s =====\n%s\n", c.Name, c.content)
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
		if strings.HasPrefix(name, ".") {
			continue
		}
		dir := filepath.Join(s.dir, name)
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() { // Stat следует за симлинками (k8s ConfigMap)
			continue
		}
		c := s.load(name, dir)
		if c.Error != "" {
			// Битый/недописанный файл не должен убирать pipeline у работающих Alloy.
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
				s.log.Warn("config disabled", "config", name, "reason", c.Error)
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
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) load(name, dir string) *Config {
	c := &Config{Name: name, Labels: map[string]LabelValues{}, Files: []string{}}

	files, _ := filepath.Glob(filepath.Join(dir, "*.alloy"))
	sort.Strings(files)
	var b strings.Builder
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			c.Error = "read " + filepath.Base(f) + ": " + err.Error()
			return c
		}
		if fi, err := os.Stat(f); err == nil && fi.ModTime().After(c.Modified) {
			c.Modified = fi.ModTime()
		}
		c.Files = append(c.Files, filepath.Base(f))
		b.WriteString(strings.TrimRight(string(data), "\n"))
		b.WriteString("\n")
	}
	c.content = b.String()
	c.Size = len(c.content)
	c.Hash = sha(c.content)[:12]

	if len(files) == 0 {
		c.Error = "no *.alloy files"
		return c
	}

	raw, err := os.ReadFile(filepath.Join(dir, "labels.yaml"))
	if err != nil {
		c.Error = "labels.yaml is missing (use `labels: {}` to match every Alloy)"
		return c
	}
	var lf labelsFile
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&lf); err == io.EOF {
		c.Error = "labels.yaml is empty (use `labels: {}` to match every Alloy)"
		return c
	} else if err != nil {
		c.Error = "labels.yaml: " + err.Error()
		return c
	}
	if lf.Labels != nil {
		c.Labels = lf.Labels
	}
	c.Description = lf.Description
	return c
}
