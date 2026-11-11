package codeindex

import (
	"path/filepath"
	"strings"
)

type Ignore struct {
	DirNames     map[string]bool
	FileSuffixes map[string]bool
	Patterns     []string
	MaxFileBytes int64
}

func DefaultIgnore() Ignore {
	dirs := []string{".git", "node_modules", "vendor", "dist", "build", "bin", "tmp", "coverage", ".harness"}
	suffixes := []string{".min.js", ".lock", ".png", ".jpg", ".jpeg", ".gif", ".ico",
		".zip", ".tar", ".gz", ".pdf", ".exe", ".dll", ".so", ".dylib", ".woff", ".woff2", ".ttf"}
	ig := Ignore{
		DirNames:     map[string]bool{},
		FileSuffixes: map[string]bool{},
		Patterns:     []string{"/.harness/tmp/", "/.git/"},
		MaxFileBytes: 2 << 20,
	}
	for _, d := range dirs {
		ig.DirNames[d] = true
	}
	for _, s := range suffixes {
		ig.FileSuffixes[s] = true
	}
	return ig
}

func (i Ignore) ShouldIgnore(path string) bool {
	normalized := filepath.ToSlash(strings.ToLower(path))
	if normalized == "" {
		return true
	}
	for _, pattern := range i.Patterns {
		if strings.Contains(normalized, strings.ToLower(pattern)) {
			return true
		}
	}
	segments := strings.Split(normalized, "/")
	for _, segment := range segments {
		if i.DirNames[segment] {
			return true
		}
	}
	base := segments[len(segments)-1]
	for suffix := range i.FileSuffixes {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return false
}

func (i Ignore) TooLarge(size int64) bool {
	return i.MaxFileBytes > 0 && size > i.MaxFileBytes
}

func (i Ignore) ShouldWatchDir(path string) bool {
	normalized := filepath.ToSlash(strings.ToLower(path))
	segments := strings.Split(normalized, "/")
	for _, segment := range segments {
		if i.DirNames[segment] {
			return false
		}
	}
	return true
}
