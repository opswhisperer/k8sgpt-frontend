package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := loadConfig(filepath.Join("..", "..", "config", "example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) == 0 || cfg.Rules[0].ID != "config-1" || cfg.Rules[0].From != "config" {
		t.Errorf("rules = %+v", cfg.Rules)
	}
	sa := res("Security/ServiceAccount", "apps", "default", "Default service account is being used by pods: [x]")
	if !cfg.Rules[0].Match(&sa) {
		t.Error("first example rule should match a default-SA finding")
	}
}

func TestConfigErrors(t *testing.T) {
	if cfg, err := loadConfig(filepath.Join(t.TempDir(), "missing.yaml")); err != nil || len(cfg.Rules) != 0 {
		t.Errorf("missing file: %v %v", cfg, err)
	}
	for name, body := range map[string]string{
		"bad regex":     "rules:\n  - name_regex: '('\n",
		"empty rule":    "rules:\n  - note: nothing\n",
		"unknown field": "rules:\n  - knd: Pod\n",
	} {
		p := filepath.Join(t.TempDir(), "c.yaml")
		_ = os.WriteFile(p, []byte(body), 0o644)
		if _, err := loadConfig(p); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
