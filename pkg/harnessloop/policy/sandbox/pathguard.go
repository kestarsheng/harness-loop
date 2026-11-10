package sandbox

import (
	"fmt"
	"path/filepath"
	"strings"
)

type PathGuard struct {
	Root            string
	AllowedSubpaths []string
}

func (g PathGuard) Check(paths []string) (string, error) {
	for _, p := range paths {
		resolved, err := Resolve(g.Root, p)
		if err != nil {
			return p, err
		}
		if len(g.AllowedSubpaths) > 0 && !g.underAllowed(resolved) {
			return p, fmt.Errorf("%w: %q is outside allowed subpaths", ErrPathEscape, p)
		}
	}
	return "", nil
}

func (g PathGuard) underAllowed(resolved string) bool {
	rootAbs, err := filepath.Abs(g.Root)
	if err != nil {
		return false
	}
	for _, sub := range g.AllowedSubpaths {
		allowed := filepath.Clean(filepath.Join(rootAbs, filepath.FromSlash(sub)))
		allowedNorm := normalize(allowed)
		if normalize(resolved) == allowedNorm || strings.HasPrefix(normalize(resolved), allowedNorm+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
