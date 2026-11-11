package pgindex

import (
	"strings"
	"unicode"
)

var stopwords = map[string]bool{
	"the": true, "a": true, "an": true, "of": true, "to": true, "and": true, "or": true,
	"is": true, "are": true, "be": true, "in": true, "on": true, "for": true, "with": true,
	"的": true, "了": true, "是": true, "在": true, "和": true, "与": true, "及": true,
}

func Tokenize(input string) string {
	tokens := tokenize(input)
	return strings.Join(tokens, " ")
}

func tokenize(input string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(tok string) {
		tok = strings.ToLower(strings.TrimSpace(tok))
		if tok == "" || stopwords[tok] || seen[tok] {
			return
		}
		seen[tok] = true
		out = append(out, tok)
	}

	for _, segment := range splitSegments(input) {
		if isHanSegment(segment) {
			for _, gram := range hanGrams(segment) {
				add(gram)
			}
			continue
		}
		for _, word := range splitCamel(segment) {
			add(word)
		}
	}
	return out
}

func splitSegments(s string) []string {
	var segments []string
	var current []rune
	flush := func() {
		if len(current) > 0 {
			segments = append(segments, string(current))
			current = nil
		}
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.Han, r) {
			current = append(current, r)
			continue
		}
		flush()
	}
	flush()
	return segments
}

func isHanSegment(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func hanGrams(s string) []string {
	runes := []rune(s)
	out := make([]string, 0, len(runes)*3)
	if len(runes) == 1 {
		return []string{s}
	}
	for i := 0; i < len(runes); i++ {
		if i+2 <= len(runes) {
			out = append(out, string(runes[i:i+2]))
		}
		if i+3 <= len(runes) {
			out = append(out, string(runes[i:i+3]))
		}
	}
	return out
}

func splitCamel(word string) []string {
	runes := []rune(word)
	if len(runes) == 0 {
		return nil
	}
	var (
		out     []string
		current []rune
	)
	flush := func() {
		if len(current) > 0 {
			out = append(out, string(current))
			current = nil
		}
	}
	for i, r := range runes {
		if i == 0 {
			current = append(current, r)
			continue
		}
		prev := runes[i-1]
		nextIsLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
		if unicode.IsUpper(r) {
			if unicode.IsLower(prev) || unicode.IsDigit(prev) {
				flush()
			} else if unicode.IsUpper(prev) && nextIsLower {
				flush()
			}
		}
		current = append(current, r)
	}
	flush()
	return out
}

func TokensForTsv(input string) string {
	return Tokenize(input)
}
