package sandbox

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrPathEscape = errors.New("E_PATH_ESCAPE")
	ErrUnsafePath = errors.New("E_UNSAFE_PATH")
)

func Resolve(root, rel string) (string, error) {
	if strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("%w: NUL byte in path", ErrUnsafePath)
	}
	if strings.HasPrefix(rel, `\\`) || strings.HasPrefix(rel, "//") {
		return "", fmt.Errorf("%w: UNC path is not allowed", ErrUnsafePath)
	}
	decoded := rel
	if unescaped, err := url.PathUnescape(rel); err == nil {
		decoded = unescaped
	}
	decoded = strings.ReplaceAll(decoded, `\`, "/")
	if decoded == "" || decoded == "." {
		return "", fmt.Errorf("%w: empty path", ErrUnsafePath)
	}
	if filepath.IsAbs(decoded) || strings.HasPrefix(decoded, "/") || strings.HasPrefix(decoded, `\`) {
		return "", fmt.Errorf("%w: absolute path %q is not allowed", ErrUnsafePath, rel)
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("%w: workspace root: %v", ErrUnsafePath, err)
	}
	rootResolved, err := evalBestEffort(rootAbs)
	if err != nil {
		rootResolved = rootAbs
	}

	target := filepath.Clean(filepath.Join(rootResolved, filepath.FromSlash(decoded)))
	resolved, err := evalBestEffort(target)
	if err != nil {
		return "", fmt.Errorf("%w: resolve %q: %v", ErrUnsafePath, rel, err)
	}
	if !isWithin(rootResolved, resolved) {
		return "", fmt.Errorf("%w: %q escapes workspace root", ErrPathEscape, rel)
	}
	return resolved, nil
}

func evalBestEffort(target string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		return resolved, nil
	}
	dir := filepath.Dir(target)
	if dir == target {
		return target, nil
	}
	parent, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return target, nil
	}
	return filepath.Join(parent, filepath.Base(target)), nil
}

func isWithin(root, target string) bool {
	root = normalize(root)
	target = normalize(target)
	if target == root {
		return true
	}
	return strings.HasPrefix(target, root+string(filepath.Separator))
}

func normalize(p string) string {
	p = filepath.Clean(p)
	if os.PathSeparator == '\\' {
		p = strings.ToLower(p)
	}
	return p
}

func IsEscape(err error) bool {
	return errors.Is(err, ErrPathEscape)
}
