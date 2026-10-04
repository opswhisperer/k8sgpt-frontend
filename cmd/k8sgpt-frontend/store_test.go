package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStorePersists(t *testing.T) {
	dir := t.TempDir()
	s := openStore(dir)
	if !s.Writable() || s.HadState() {
		t.Fatalf("fresh store: writable=%v hadState=%v", s.Writable(), s.HadState())
	}
	if err := s.AddIgnores([]Ignore{{Key: "Pod/apps/web", Note: "n"}}); err != nil {
		t.Fatal(err)
	}
	rule, err := s.AddRule(Rule{Kind: "Security/*", Note: "noise"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddRule(Rule{NameRegex: "("}); err == nil {
		t.Error("invalid rule accepted")
	}
	_ = s.UpdateState(func(st *State) { st.Notified["Pod/apps/web"] = time.Now() })

	s2 := openStore(dir)
	if _, ok := s2.Ignores()["Pod/apps/web"]; !ok {
		t.Error("ignore not persisted")
	}
	rules := s2.Rules()
	if len(rules) != 1 || rules[0].ID != rule.ID || rules[0].From != "dashboard" {
		t.Fatalf("rules = %+v", rules)
	}
	pod := res("Pod", "apps", "web")
	sec := res("Security/Pod", "apps", "web")
	if !rules[0].Match(&sec) || rules[0].Match(&pod) {
		t.Error("reloaded rule not compiled correctly")
	}
	if !s2.HadState() {
		t.Error("state not persisted")
	}

	if ok, _ := s2.DeleteRule(rule.ID); !ok {
		t.Error("delete failed")
	}
	if ok, _ := s2.DeleteRule(rule.ID); ok {
		t.Error("second delete should report not found")
	}
	if n, _ := s2.RemoveIgnores([]string{"Pod/apps/web", "nope"}); n != 1 {
		t.Errorf("removed %d", n)
	}
	if entries, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(entries) != 0 {
		t.Errorf("temp files left behind: %v", entries)
	}
}

func TestStoreReadOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	s := openStore(dir)
	if s.Writable() {
		t.Fatal("expected read-only store")
	}
	if err := s.AddIgnores([]Ignore{{Key: "k"}}); !errors.Is(err, ErrReadOnly) {
		t.Errorf("AddIgnores err = %v", err)
	}
	if _, err := s.AddRule(Rule{Kind: "Pod"}); !errors.Is(err, ErrReadOnly) {
		t.Errorf("AddRule err = %v", err)
	}
	// State still works in memory.
	if err := s.UpdateState(func(st *State) { st.FirstSeen["k"] = time.Now() }); err != nil {
		t.Errorf("UpdateState err = %v", err)
	}
}
