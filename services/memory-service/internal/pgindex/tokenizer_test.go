package pgindex

import (
	"strings"
	"testing"
)

func TestTokenizeSplitsIdentifiers(t *testing.T) {
	cases := map[string][]string{
		"getUserProfile":   {"get", "user", "profile"},
		"get_user_profile": {"get", "user", "profile"},
		"get-user-profile": {"get", "user", "profile"},
		"HTTPServer":       {"http", "server"},
		"a/b.go":           {"b", "go"},
		"OrderVersion42":   {"order", "version42"},
	}
	for input, want := range cases {
		got := tokenize(input)
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("%q -> %v, want %v", input, got, want)
		}
	}
}

func TestTokenizeChineseGrams(t *testing.T) {
	got := tokenize("用户偏好设置")
	joined := strings.Join(got, " ")
	for _, want := range []string{"用户", "偏好", "设置", "用户偏", "户偏好", "偏好设", "好设置"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing gram %q in %v", want, got)
		}
	}
	if strings.Contains(joined, "用户偏好设置") {
		t.Fatalf("full Chinese string must not be a single token: %v", got)
	}
}

func TestTokenizeFiltersStopwordsAndDedupes(t *testing.T) {
	got := tokenize("the User user profile 的 用户")
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "the") {
		t.Fatalf("english stopword kept: %v", got)
	}
	if strings.Contains(joined, "的") {
		t.Fatalf("chinese stopword kept: %v", got)
	}
	count := 0
	for _, tok := range got {
		if tok == "user" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("duplicate token kept: %v", got)
	}
}

func TestTokenizeEmptyAndSymbols(t *testing.T) {
	for _, input := range []string{"", "   ", "!!!", "的"} {
		if got := Tokenize(input); got != "" {
			t.Fatalf("%q -> %q, want empty", input, got)
		}
	}
}
