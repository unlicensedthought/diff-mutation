// Package config loads changeling.json (scopes + oracle map) and
// equivalents.json (mutants marked ignored, each with a reason).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Override adjusts oracle discovery for one source file.
type Override struct {
	// Add: tests that exercise the file without naming it (a wrapper calls it).
	Add []string `json:"add,omitempty"`
	// Exclude: tests that name the file but cannot be an oracle for it; each
	// needs a reason in Why.
	Exclude []string `json:"exclude,omitempty"`
	// Only: when set, discovery is replaced by exactly this list.
	Only []string `json:"only,omitempty"`
	Why  string   `json:"why,omitempty"`
}

// Config is changeling.json.
type Config struct {
	Scopes struct {
		Bash []string `json:"bash"`
		TS   []string `json:"ts"`
		Go   []string `json:"go"`
	} `json:"scopes"`
	Oracle struct {
		TestGlobs []string            `json:"testGlobs"`
		Runner    []string            `json:"runner"`
		Overrides map[string]Override `json:"overrides"`
	} `json:"oracle"`
	NumericSkipCommands []string `json:"numericSkipCommands"`
	Equivalents         string   `json:"equivalents"`
	Gremlins            struct {
		Module  string `json:"module"`
		Version string `json:"version"`
	} `json:"gremlins"`
}

// Equivalent marks one mutant id as ignored.
type Equivalent struct {
	ID     string `json:"id"`
	File   string `json:"file"`
	Reason string `json:"reason"`
	// Original/Replacement document what the id names, so a reviewer can read
	// the file without running the tool.
	Function    string `json:"function,omitempty"`
	Operator    string `json:"operator,omitempty"`
	Original    string `json:"original,omitempty"`
	Replacement string `json:"replacement,omitempty"`
}

// Load reads the config at path (repo-relative or absolute).
func Load(repo, path string) (*Config, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(repo, path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(c.Oracle.Runner) == 0 {
		c.Oracle.Runner = []string{"bash"}
	}
	for f, o := range c.Oracle.Overrides {
		if len(o.Exclude) > 0 && strings.TrimSpace(o.Why) == "" {
			return nil, fmt.Errorf("%s: override for %s excludes tests without a why", path, f)
		}
	}
	return &c, nil
}

// LoadEquivalents reads the equivalents file; every entry needs a reason.
func LoadEquivalents(repo, path string) (map[string]Equivalent, error) {
	out := map[string]Equivalent{}
	if path == "" {
		return out, nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(repo, path)
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	var list struct {
		Equivalents []Equivalent `json:"equivalents"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, e := range list.Equivalents {
		if e.ID == "" || strings.TrimSpace(e.Reason) == "" {
			return nil, fmt.Errorf("%s: equivalent %q has no reason; an ignored mutant must say why", path, e.ID)
		}
		out[e.File+"#"+e.ID] = e
	}
	return out, nil
}

// InScope reports whether rel matches one of the scope entries (exact file or
// directory prefix ending in /).
func InScope(rel string, scopes []string) bool {
	for _, s := range scopes {
		if s == rel || (strings.HasSuffix(s, "/") && strings.HasPrefix(rel, s)) {
			return true
		}
	}
	return false
}
