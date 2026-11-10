package service

import "testing"

func TestIsStageMode(t *testing.T) {
	cases := []struct {
		name string
		vars map[string]any
		want bool
	}{
		{"missing", map[string]any{}, false},
		{"bool true", map[string]any{"__stage_mode__": true}, true},
		{"bool false", map[string]any{"__stage_mode__": false}, false},
		{"string true", map[string]any{"__stage_mode__": "true"}, true},
		{"string TRUE", map[string]any{"__stage_mode__": " TRUE "}, true},
		{"string 1", map[string]any{"__stage_mode__": "1"}, true},
		{"string yes", map[string]any{"__stage_mode__": "yes"}, true},
		{"string false", map[string]any{"__stage_mode__": "false"}, false},
		{"string 0", map[string]any{"__stage_mode__": "0"}, false},
		{"other type", map[string]any{"__stage_mode__": 1}, false},
		{"nil value", map[string]any{"__stage_mode__": nil}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isStageMode(tc.vars); got != tc.want {
				t.Fatalf("isStageMode(%v) = %v, want %v", tc.vars, got, tc.want)
			}
		})
	}
}
