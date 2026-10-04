package main

import (
	"errors"
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// Config is the optional config file (see config/example.yaml). It holds rules
// that are managed in git rather than from the dashboard.
type Config struct {
	Rules []Rule `json:"rules"`
}

// loadConfig reads the config file. A missing file is not an error.
func loadConfig(path string) (*Config, error) {
	cfg := &Config{}
	if path == "" {
		return cfg, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.UnmarshalStrict(b, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for i := range cfg.Rules {
		r := &cfg.Rules[i]
		if err := r.compile(); err != nil {
			return nil, fmt.Errorf("%s: rules[%d]: %w", path, i, err)
		}
		r.ID = fmt.Sprintf("config-%d", i+1)
		r.From = "config"
	}
	return cfg, nil
}
