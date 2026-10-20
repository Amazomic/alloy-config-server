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

// Модель работы: имя конфигурационного файла — это и есть условие выдачи.
// Разделитель — нижнее подчёркивание (никаких «=» в именах файлов):
//
//	env_prod.alloy    → лейбл env со значением prod: отдаём тем Alloy,
//	                    у кого в attributes есть env="prod"
//	id_node-1.alloy   → персональный конфиг Alloy с id="node-1"
//	*.alloy           → всем без исключения
//
// У каждого файла ровно одна такая метка: <ключ>_<значение>.alloy.
// Первый "_" разделяет ключ и значение; значение может содержать дефисы
// и точки, но не подчёркивания. Атрибуты формируются из local_attributes
// запроса remotecfg (+ виртуальный id collector'а).

// Config — один файл-конфиг с разобранным из имени условием.
type Config struct {
	Name     string    `json:"name"`  // базовое имя без .alloy, напр. "env_prod"
	Label    string    `json:"label"` // условие целиком (== Name): "<key>_<value>" или "*"
	Key      string    `json:"key,omitempty"`
	Value    string    `json:"value,omitempty"`
	Always   bool      `json:"always"` // "*": подходит всем
	Hash     string    `json:"hash"`
	Size     int       `json:"size"`
	Modified time.Time `json:"modified"`
	Error    string    `json:"error,omitempty"`
	Warning  string    `json:"warning,omitempty"`
	content  string
}

// Matches: файлу с "<key>_<value>" нужен атрибут key со значением value;
// "*" совпадает всегда.
func (c *Config) Matches(attrs map[string]string) bool {
	if c.Error != "" {
		return false
	}
	if c.Always {
		return true
	}
	got, ok := attrs[c.Key]
	return ok && c.Value == got
}

// parseName разбирает базовое имя файла: "<key>_<value>" или "*".
// Ровно одна метка: первый "_" разделяет ключ и значение.
func parseName(name string) (key, value string, always bool, ok bool) {
	if name == "*" {
		return "", "", true, true
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

// Select возвращает конфиги, чьё условие из имени совпало с атрибутами Alloy.
// Порядок — по именам файлов; "*" идёт первым, чтобы базовый конфиг был в начале.
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
		fname := e.Name()
		if strings.HasPrefix(fname, ".") || e.IsDir() || !strings.HasSuffix(fname, ".alloy") {
			continue
		}
		name := strings.TrimSuffix(fname, ".alloy")
		key, value, always, ok := parseName(name)
		if !ok {
			s.warnOnce(name, "bad filename: expected \"<key>_<value>.alloy\" or \"*.alloy\"")
			continue
		}
		c := s.loadFile(name, key, value, always, filepath.Join(s.dir, fname))
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
			return out[i].Always // "*" первым
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (s *Store) warnOnce(name, msg string) {
	if prev := s.lastErr[name]; prev == msg {
		return
	}
	s.lastErr[name] = msg
	s.log.Warn("skipping file", "name", name, "reason", msg)
}

func (s *Store) loadFile(name, key, value string, always bool, path string) *Config {
	c := &Config{Name: name, Label: name, Key: key, Value: value, Always: always}
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
