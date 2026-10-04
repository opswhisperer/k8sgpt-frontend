package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ErrReadOnly is returned by write operations when the data directory is not writable.
var ErrReadOnly = errors.New("data directory is not writable")

// State is the poller's memory, persisted so a restart does not re-notify.
type State struct {
	// FirstSeen records when each finding was first observed.
	FirstSeen map[string]time.Time `json:"first_seen"`
	// Notified records findings already notified, or deliberately not notified
	// because they were hidden when they became due (Suppressed).
	Notified   map[string]time.Time `json:"notified"`
	Suppressed map[string]bool      `json:"suppressed,omitempty"`
	Health     HealthState          `json:"health"`
}

// HealthState is the last health status an alert was sent for.
type HealthState struct {
	Status  string    `json:"status,omitempty"`
	Since   time.Time `json:"since,omitempty"`
	Summary string    `json:"summary,omitempty"`
}

// Store keeps ignores, dashboard rules and poller state as JSON files in one
// directory. Every change rewrites the file atomically (temp file + rename).
// When the directory is not writable everything still works in memory, but
// ignore/rule changes are refused so they are not silently lost on restart.
type Store struct {
	dir      string
	writable bool

	mu       sync.Mutex
	ignores  map[string]Ignore
	rules    []Rule
	state    State
	hadState bool
}

const (
	ignoresFile = "ignores.json"
	rulesFile   = "rules.json"
	stateFile   = "state.json"
)

func openStore(dir string) *Store {
	s := &Store{dir: dir, ignores: map[string]Ignore{}}
	s.state = State{FirstSeen: map[string]time.Time{}, Notified: map[string]time.Time{}, Suppressed: map[string]bool{}}
	if dir == "" {
		log.Printf("store: no data directory; ignores, rules and notification state are kept in memory only")
		return s
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("store: %v; running read-only", err)
	} else if err := probeWritable(dir); err != nil {
		log.Printf("store: %s is not writable (%v); running read-only", dir, err)
	} else {
		s.writable = true
	}
	s.load(ignoresFile, &s.ignores)
	s.load(rulesFile, &s.rules)
	s.hadState = s.load(stateFile, &s.state)
	if s.ignores == nil {
		s.ignores = map[string]Ignore{}
	}
	if s.state.FirstSeen == nil {
		s.state.FirstSeen = map[string]time.Time{}
	}
	if s.state.Notified == nil {
		s.state.Notified = map[string]time.Time{}
	}
	if s.state.Suppressed == nil {
		s.state.Suppressed = map[string]bool{}
	}
	valid := s.rules[:0]
	for _, r := range s.rules {
		if err := r.compile(); err != nil {
			log.Printf("store: dropping invalid rule %s: %v", r.ID, err)
			continue
		}
		r.From = "dashboard"
		valid = append(valid, r)
	}
	s.rules = valid
	return s
}

func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// load reads a JSON file into v and reports whether the file existed.
func (s *Store) load(name string, v interface{}) bool {
	b, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("store: read %s: %v", name, err)
		}
		return false
	}
	if err := json.Unmarshal(b, v); err != nil {
		log.Printf("store: parse %s: %v", name, err)
		return false
	}
	return true
}

// save writes v to name atomically. Callers hold s.mu.
func (s *Store) save(name string, v interface{}) error {
	if !s.writable {
		return ErrReadOnly
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, name+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, name))
}

func (s *Store) Writable() bool { return s.writable }

// HadState reports whether state.json existed at startup.
func (s *Store) HadState() bool { return s.hadState }

func (s *Store) Ignores() map[string]Ignore {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Ignore, len(s.ignores))
	for k, v := range s.ignores {
		out[k] = v
	}
	return out
}

func (s *Store) Rules() []Rule {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Rule(nil), s.rules...)
}

func (s *Store) AddIgnores(igs []Ignore) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writable {
		return ErrReadOnly
	}
	for _, ig := range igs {
		s.ignores[ig.Key] = ig
	}
	return s.save(ignoresFile, s.ignores)
}

func (s *Store) RemoveIgnores(keys []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writable {
		return 0, ErrReadOnly
	}
	n := 0
	for _, k := range keys {
		if _, ok := s.ignores[k]; ok {
			delete(s.ignores, k)
			n++
		}
	}
	return n, s.save(ignoresFile, s.ignores)
}

func (s *Store) AddRule(r Rule) (Rule, error) {
	if err := r.compile(); err != nil {
		return Rule{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writable {
		return Rule{}, ErrReadOnly
	}
	now := time.Now().UTC()
	r.ID, r.At, r.From = newID(), &now, "dashboard"
	s.rules = append(s.rules, r)
	return r, s.save(rulesFile, s.rules)
}

func (s *Store) DeleteRule(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writable {
		return false, ErrReadOnly
	}
	for i, r := range s.rules {
		if r.ID == id {
			s.rules = append(s.rules[:i], s.rules[i+1:]...)
			return true, s.save(rulesFile, s.rules)
		}
	}
	return false, nil
}

// FirstSeen returns a copy of the first-seen times.
func (s *Store) FirstSeen() map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]time.Time, len(s.state.FirstSeen))
	for k, v := range s.state.FirstSeen {
		out[k] = v
	}
	return out
}

// UpdateState runs fn on the state and persists it. Persistence failures are
// returned but the in-memory change stands.
func (s *Store) UpdateState(fn func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.state)
	if !s.writable {
		return nil
	}
	if err := s.save(stateFile, s.state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

func (s *Store) HealthState() HealthState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Health
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
