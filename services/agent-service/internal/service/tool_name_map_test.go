package service

import "testing"

func TestHarnessToolNameMapsMCPNames(t *testing.T) {
	cases := map[string]string{
		"file_write":       "write_file",
		"file_read":        "read_file",
		"code_execute":     "run_command",
		"write_file":       "write_file",
		"knowledge_search": "knowledge_search",
	}
	for in, want := range cases {
		if got := harnessToolName(in); got != want {
			t.Fatalf("harnessToolName(%q) = %q, want %q", in, got, want)
		}
	}
}
