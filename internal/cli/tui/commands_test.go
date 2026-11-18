package tui

import "testing"

func TestDispatchSessionLine(t *testing.T) {
	cases := []struct {
		line   string
		action SessionAction
		goal   string
	}{
		{"", SessionNoop, ""},
		{"   ", SessionNoop, ""},
		{"exit", SessionExit, ""},
		{"  EXIT  ", SessionExit, ""},
		{"/exit", SessionExit, ""},
		{"/harness loop exit", SessionExit, ""},
		{"/harness-loop exit", SessionExit, ""},
		{"/quit", SessionExit, ""},
		{"quit smoking", SessionSubmit, "quit smoking"},
		{" 修复 bug ", SessionSubmit, "修复 bug"},
	}
	for _, tc := range cases {
		got := DispatchSessionLine(tc.line)
		if got.Action != tc.action {
			t.Fatalf("DispatchSessionLine(%q).Action = %v, want %v", tc.line, got.Action, tc.action)
		}
		if tc.action == SessionSubmit && got.Goal != tc.goal {
			t.Fatalf("DispatchSessionLine(%q).Goal = %q, want %q", tc.line, got.Goal, tc.goal)
		}
	}
}
