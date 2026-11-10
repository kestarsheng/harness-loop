package output

import (
	"regexp"
	"strings"
)

var redactPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{8,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]+`),
	regexp.MustCompile(`(?i)password\s*=\s*\S+`),
	regexp.MustCompile(`(?i)api[_-]?key["']?\s*[:=]\s*["']?[A-Za-z0-9_\-]{8,}`),
}

const mask = "***REDACTED***"

func Redact(s string) string {
	out := s
	for _, p := range redactPatterns {
		out = p.ReplaceAllString(out, mask)
	}
	return out
}

func RedactAny(v any) any {
	switch t := v.(type) {
	case string:
		return Redact(t)
	case []string:
		out := make([]string, len(t))
		for i, s := range t {
			out[i] = Redact(s)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if isSecretKey(k) {
				out[k] = mask
				continue
			}
			out[k] = RedactAny(val)
		}
		return out
	default:
		return v
	}
}

func isSecretKey(key string) bool {
	k := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	switch k {
	case "api_key", "apikey", "password", "secret", "token", "authorization", "credential", "api_key_ref":
		return true
	}
	return false
}
