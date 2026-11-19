package sandbox

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	ErrPathEscape = errors.New("E_PATH_ESCAPE")
	ErrUnsafePath = errors.New("E_UNSAFE_PATH")
)

func Resolve(root, rel string) (string, error) {
	return resolveWith(root, rel, evalBestEffort)
}

func ResolveCached(root, rel string) (string, error) {
	return resolveWith(root, rel, evalBestEffortCached)
}

func resolveWith(root, rel string, eval func(string) (string, error)) (string, error) {
	if strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("%w: NUL byte in path", ErrUnsafePath)
	}
	// UNC paths (including `\\?\` and `\\.\` device forms) name a share or a
	// device, not a file inside the workspace, so they stay refused.
	if isUNCPath(rel) {
		return "", fmt.Errorf("%w: UNC path is not allowed", ErrUnsafePath)
	}
	decoded := rel
	if unescaped, err := url.PathUnescape(rel); err == nil {
		decoded = unescaped
	}
	decoded = strings.ReplaceAll(decoded, `\`, "/")
	// "." and "" are not the same argument. "." has a referent — it names the
	// workspace root, which is exactly what an agent means by `read_file(".")`
	// or `list_dir(".")`. "" has none: nothing was named, so refusing it keeps
	// the "a path argument must name something" contract intact.
	if decoded == "" {
		return "", fmt.Errorf("%w: empty path", ErrUnsafePath)
	}
	// Re-check after percent-decoding so `%5c%5cserver` cannot smuggle a UNC
	// path past the raw-prefix check above.
	if isUNCPath(decoded) {
		return "", fmt.Errorf("%w: UNC path is not allowed", ErrUnsafePath)
	}
	if hasURLScheme(decoded) {
		return "", fmt.Errorf("%w: url-style path %q is not allowed", ErrUnsafePath, rel)
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("%w: workspace root: %v", ErrUnsafePath, err)
	}
	rootResolved, err := eval(rootAbs)
	if err != nil {
		rootResolved = rootAbs
	}

	// Absolute paths are accepted only when they already point inside the
	// workspace. The agent often names the artifact the way the user did
	// ("D:\Desktop\morning_greeting.vbs") while workspace_root is "D:\Desktop";
	// rebasing that onto the root would look for a nested copy, and refusing it
	// made every /task write against a desktop workspace fail. Normalize
	// separators and case, resolve links, then apply the same containment test
	// as relative paths. Anything outside the root keeps E_UNSAFE_PATH.
	if filepath.IsAbs(decoded) {
		target := filepath.Clean(filepath.FromSlash(decoded))
		resolved, err := eval(target)
		if err != nil {
			return "", fmt.Errorf("%w: resolve %q: %v", ErrUnsafePath, rel, err)
		}
		if !isWithin(rootResolved, resolved) {
			// A lexical path inside the root that resolves outside is a
			// symlink/junction escape, not a user naming an outside absolute
			// path. Keep the same sentinel the relative-path branch uses.
			if isWithin(rootResolved, target) {
				return "", fmt.Errorf("%w: %q escapes workspace root via a link", ErrPathEscape, rel)
			}
			return "", fmt.Errorf("%w: absolute path %q is not allowed", ErrUnsafePath, rel)
		}
		return resolved, nil
	}
	// Root-relative paths without a volume ("/etc/passwd", `\Windows`) are
	// absolute in intent but not filepath.IsAbs on Windows; keep them refused.
	if strings.HasPrefix(decoded, "/") || strings.HasPrefix(decoded, `\`) {
		return "", fmt.Errorf("%w: absolute path %q is not allowed", ErrUnsafePath, rel)
	}
	// Parent traversal is refused here rather than left to the escapes-root
	// check below: when the root is itself a filesystem root (`/`, `C:\`),
	// joining `..` stays "inside" and the containment test would wave it
	// through. `.` is unaffected — Clean(".") is ".", not "..".
	if hasParentTraversal(decoded) {
		return "", fmt.Errorf("%w: %q escapes workspace root", ErrPathEscape, rel)
	}

	target := filepath.Clean(filepath.Join(rootResolved, filepath.FromSlash(decoded)))
	resolved, err := eval(target)
	if err != nil {
		return "", fmt.Errorf("%w: resolve %q: %v", ErrUnsafePath, rel, err)
	}
	if !isWithin(rootResolved, resolved) {
		return "", fmt.Errorf("%w: %q escapes workspace root", ErrPathEscape, rel)
	}
	return resolved, nil
}

// isUNCPath reports whether p names a UNC share or a `\\?\` / `\\.\` device
// path. Both separator spellings are checked because callers may hand the guard
// either form and because decoding can turn `%5c` into a backslash.
func isUNCPath(p string) bool {
	return strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//")
}

// hasParentTraversal reports whether a slash-normalized relative path climbs
// above the workspace root. Only leading `..` segments count: a segment such as
// ".. " is an ordinary file name on most filesystems, and `pkg/..` cleans to
// "." (inside the root), so both stay allowed.
func hasParentTraversal(p string) bool {
	cleaned := filepath.Clean(filepath.FromSlash(p))
	return cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator))
}

func hasURLScheme(p string) bool {
	idx := strings.Index(p, "://")
	if idx <= 0 {
		return false
	}
	scheme := p[:idx]
	for i := 0; i < len(scheme); i++ {
		c := scheme[i]
		isAlpha := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		isDigit := c >= '0' && c <= '9'
		if i == 0 && !isAlpha {
			return false
		}
		if !isAlpha && !isDigit && c != '+' && c != '-' && c != '.' {
			return false
		}
	}
	return true
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

type cachedRealpath struct {
	resolved string
	expires  time.Time
}

var realpathCache sync.Map

func evalBestEffortCached(target string) (string, error) {
	if entry, ok := realpathCache.Load(target); ok {
		cached, _ := entry.(cachedRealpath)
		if time.Now().Before(cached.expires) {
			return cached.resolved, nil
		}
		realpathCache.Delete(target)
	}

	if _, err := os.Lstat(target); err == nil {
		resolved, err := filepath.EvalSymlinks(target)
		if err == nil {
			realpathCache.Store(target, cachedRealpath{resolved: resolved, expires: time.Now().Add(time.Minute)})
			return resolved, nil
		}
	}

	dir := filepath.Dir(target)
	if dir == target {
		return target, nil
	}
	parent, err := evalBestEffortCached(dir)
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
