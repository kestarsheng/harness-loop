package tui

import (
	"strings"
	"testing"
)

func TestOpenersFor(t *testing.T) {
	url := "http://localhost:5174/harness-loop/monitor"

	windows := openersFor("windows", url)
	if len(windows) < 1 {
		t.Fatal("windows must have at least one opener")
	}
	if windows[0].Name != "rundll32" {
		t.Fatalf("windows primary opener = %q", windows[0].Name)
	}
	if got := strings.Join(windows[0].Args, " "); got != "url.dll,FileProtocolHandler "+url {
		t.Fatalf("windows rundll32 args = %q", got)
	}
	foundStart := false
	for _, candidate := range windows[1:] {
		if candidate.Name == "cmd" && strings.Join(candidate.Args, " ") == "/c start  "+url {
			foundStart = true
		}
	}
	if !foundStart {
		t.Fatalf("windows fallback `cmd /c start` missing: %+v", windows)
	}

	darwin := openersFor("darwin", url)
	if len(darwin) != 1 || darwin[0].Name != "open" || darwin[0].Args[0] != url {
		t.Fatalf("darwin opener = %+v", darwin)
	}

	linux := openersFor("linux", url)
	if len(linux) != 1 || linux[0].Name != "xdg-open" || linux[0].Args[0] != url {
		t.Fatalf("linux opener = %+v", linux)
	}
}
