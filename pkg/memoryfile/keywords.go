package memoryfile

import (
	"strings"
	"unicode"
)

var keywordStopwords = map[string]bool{
	"the": true, "a": true, "an": true, "of": true, "to": true, "and": true, "or": true,
	"is": true, "are": true, "be": true, "in": true, "on": true, "for": true, "with": true,
	"的": true, "了": true, "是": true, "在": true, "和": true, "与": true, "及": true,
}

func Keywords(text string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(tok string) {
		tok = strings.ToLower(strings.TrimSpace(tok))
		if tok == "" || keywordStopwords[tok] || seen[tok] {
			return
		}
		seen[tok] = true
		out = append(out, tok)
	}

	for _, segment := range splitKeywordSegments(text) {
		if isHan(segment) {
			runes := []rune(segment)
			if len(runes) == 1 {
				add(segment)
				continue
			}
			for i := 0; i+2 <= len(runes); i++ {
				add(string(runes[i : i+2]))
			}
			continue
		}
		for _, word := range splitKeywordCamel(segment) {
			add(word)
		}
	}
	return out
}

func splitKeywordSegments(s string) []string {
	var segments []string
	var current []rune
	flush := func() {
		if len(current) > 0 {
			segments = append(segments, string(current))
			current = nil
		}
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current = append(current, r)
			continue
		}
		flush()
	}
	flush()
	return segments
}

func isHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func splitKeywordCamel(word string) []string {
	runes := []rune(word)
	if len(runes) == 0 {
		return nil
	}
	var out []string
	var current []rune
	flush := func() {
		if len(current) > 0 {
			out = append(out, string(current))
			current = nil
		}
	}
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) {
			prev := runes[i-1]
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				flush()
			}
		}
		current = append(current, r)
	}
	flush()
	return out
}
