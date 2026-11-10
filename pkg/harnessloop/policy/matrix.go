package policy

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type StageRule struct {
	Read    []string `yaml:"read"`
	Write   []string `yaml:"write"`
	Exec    []string `yaml:"exec"`
	DenyAll bool     `yaml:"deny_all"`
}

type Matrix struct {
	Stages map[string]StageRule `yaml:"stages"`
}

func DefaultMatrix() Matrix {
	return Matrix{Stages: map[string]StageRule{
		"a": {Read: []string{"read_file", "list_dir", "grep"}, Write: nil, Exec: nil},
		"b": {Read: []string{"read_file", "list_dir", "grep"}, Write: nil, Exec: nil},
		"c": {Read: []string{"read_file", "list_dir", "grep"}, Write: []string{"write_file", "edit_file", "apply_patch", "mkdir"}, Exec: []string{"go"}},
		"d": {Read: []string{"read_file", "list_dir", "grep"}, Write: nil, Exec: []string{"go"}},
		"e": {DenyAll: true},
	}}
}

func LoadMatrix(path string) (Matrix, error) {
	m := DefaultMatrix()
	if path == "" {
		return m, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	var loaded Matrix
	if err := yaml.Unmarshal(raw, &loaded); err != nil {
		return m, err
	}
	if len(loaded.Stages) == 0 {
		return m, nil
	}
	for stage, rule := range loaded.Stages {
		if _, ok := m.Stages[stage]; !ok {
			return m, fmt.Errorf("policy: unknown stage %q in matrix", stage)
		}
		m.Stages[stage] = rule
	}
	return m, nil
}

func (m Matrix) Rule(stage string) (StageRule, bool) {
	r, ok := m.Stages[stage]
	return r, ok
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}
