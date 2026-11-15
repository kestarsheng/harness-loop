package tui

import (
	"fmt"
	"os/exec"
	"runtime"
)

type opener struct {
	Name string
	Args []string
}

// openersFor returns the platform-specific commands used to hand a URL to the
// system opener, in fallback order. It is a pure function so tests can assert
// the exact command line on every OS.
func openersFor(goos, url string) []opener {
	switch goos {
	case "windows":
		return []opener{
			{Name: "rundll32", Args: []string{"url.dll,FileProtocolHandler", url}},
			{Name: "cmd", Args: []string{"/c", "start", "", url}},
		}
	case "darwin":
		return []opener{{Name: "open", Args: []string{url}}}
	default:
		return []opener{{Name: "xdg-open", Args: []string{url}}}
	}
}

// DefaultOpenURL opens url with the system browser. Callers treat failures as
// display-only errors; this function never panics.
func DefaultOpenURL(url string) error {
	candidates := openersFor(runtime.GOOS, url)
	var firstErr error
	for _, candidate := range candidates {
		if err := exec.Command(candidate.Name, candidate.Args...).Start(); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", candidate.Name, err)
			}
			continue
		}
		return nil
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("no system opener available")
	}
	return firstErr

}
