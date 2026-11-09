// Package plan loads an import plan, the file an agent writes after reading a
// scan, and checks it against a fresh scan and the spaces the caller can see.
package plan

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Version is the plan format this CLI reads.
const Version = 1

const maxPlanBytes = 8 << 20

// Column is one column of a workbook.
type Column struct {
	Name   string   `json:"name"`
	Type   string   `json:"type"`
	Values []string `json:"values,omitempty"`
}

// PlanEntry is what the agent decided one file becomes.
type PlanEntry struct {
	Path     string   `json:"path"`
	Kind     string   `json:"kind"`
	Space    string   `json:"space,omitempty"`
	Type     string   `json:"type,omitempty"`
	Title    string   `json:"title,omitempty"`
	Slug     string   `json:"slug,omitempty"`
	Describe string   `json:"describe,omitempty"`
	Columns  []Column `json:"columns,omitempty"`
	Name     string   `json:"name,omitempty"`
	Scope    string   `json:"scope,omitempty"`
}

// Plan is a loaded plan. Root is absolute.
type Plan struct {
	Version int         `json:"version"`
	Root    string      `json:"root"`
	Entries []PlanEntry `json:"entries"`
}

// Load reads and parses a plan file. A relative root is taken from the
// plan's own folder.
func Load(file string) (Plan, error) {
	var p Plan
	f, err := os.Open(file)
	if err != nil {
		return p, fmt.Errorf("The plan %s could not be read.", file)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxPlanBytes+1))
	if err != nil || len(raw) > maxPlanBytes {
		return p, fmt.Errorf("The plan %s could not be read.", file)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Plan{}, errors.New("The plan is not valid JSON in the plan format. Check it against the scan guide.")
	}
	if dec.More() {
		return Plan{}, errors.New("The plan holds more than one JSON value.")
	}
	if p.Version != Version {
		return Plan{}, fmt.Errorf("The plan is version %d and this smrit reads version %d.", p.Version, Version)
	}
	if p.Root == "" {
		return Plan{}, errors.New("The plan names no root folder.")
	}
	if len(p.Entries) == 0 {
		return Plan{}, errors.New("The plan has no entries.")
	}
	if !filepath.IsAbs(p.Root) {
		abs, err := filepath.Abs(file)
		if err != nil {
			return Plan{}, fmt.Errorf("The plan %s could not be read.", file)
		}
		p.Root = filepath.Join(filepath.Dir(abs), p.Root)
	}
	p.Root = filepath.Clean(p.Root)
	return p, nil
}
