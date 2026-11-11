package compaction

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	LineGoal           = "goal"
	LineDone           = "done"
	LineTodo           = "todo"
	LineConstraints    = "constraints"
	LineBackground     = "background"
	LineEngineFragment = "engine_fragment"
)

var ErrInvalidJSONL = errors.New("compaction: invalid jsonl")
var ErrSecretInSummary = errors.New("compaction: summary appears to contain a secret")

var validLineTypes = map[string]bool{
	LineGoal: true, LineDone: true, LineTodo: true,
	LineConstraints: true, LineBackground: true, LineEngineFragment: true,
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{8,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]+`),
	regexp.MustCompile(`(?i)password\s*=\s*\S+`),
}

type Line struct {
	Type       string    `json:"type"`
	Phase      string    `json:"phase"`
	Summary    string    `json:"summary"`
	SourceRefs []string  `json:"source_refs"`
	Timestamp  time.Time `json:"timestamp"`
}

func BuildLines(summary Summary, phase string, ts time.Time) ([]Line, error) {
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	specs := []struct {
		typ, body, ref string
	}{
		{LineGoal, summary.Goal, "task:goal"},
		{LineDone, summary.Done, "event:stage"},
		{LineTodo, summary.Todo, "plan:current"},
		{LineConstraints, summary.Constraints, "memory:constraint"},
		{LineBackground, summary.Background, "context:history"},
	}
	lines := make([]Line, 0, len(specs))
	for _, spec := range specs {
		line := Line{
			Type:       spec.typ,
			Phase:      phase,
			Summary:    spec.body,
			SourceRefs: summary.SourceRefs[spec.typ],
			Timestamp:  ts.UTC(),
		}
		if len(line.SourceRefs) == 0 {
			line.SourceRefs = []string{spec.ref}
		}
		if err := ValidateLine(line); err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	return lines, nil
}

func ValidateLine(line Line) error {
	if !validLineTypes[line.Type] {
		return fmt.Errorf("%w: unknown type %q", ErrInvalidJSONL, line.Type)
	}
	if strings.TrimSpace(line.Summary) == "" && line.Type != LineBackground {
		return fmt.Errorf("%w: %s summary must not be empty", ErrInvalidJSONL, line.Type)
	}
	if len(line.SourceRefs) == 0 {
		return fmt.Errorf("%w: %s source_refs must not be empty", ErrInvalidJSONL, line.Type)
	}
	if strings.ContainsRune(line.Summary, '\n') && line.Type == LineBackground {
		// multi-line background is allowed, but must stay valid JSON later
	}
	for _, pattern := range secretPatterns {
		if pattern.MatchString(line.Summary) {
			return fmt.Errorf("%w: type=%s", ErrSecretInSummary, line.Type)
		}
	}
	return nil
}

func EncodeJSONL(lines []Line) ([]byte, error) {
	var buf bytes.Buffer
	for _, line := range lines {
		if err := ValidateLine(line); err != nil {
			return nil, err
		}
		raw, err := json.Marshal(line)
		if err != nil {
			return nil, fmt.Errorf("compaction: encode line: %w", err)
		}
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

func DecodeJSONL(raw []byte) ([]Line, error) {
	var lines []Line
	for i, text := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if strings.TrimSpace(text) == "" {
			continue
		}
		var line Line
		if err := json.Unmarshal([]byte(text), &line); err != nil {
			return nil, fmt.Errorf("%w: line %d is not json: %v", ErrInvalidJSONL, i+1, err)
		}
		if err := ValidateLine(line); err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	return lines, nil
}

func ValidateFiveElements(lines []Line) error {
	seen := map[string]bool{}
	engineFragments := 0
	for _, line := range lines {
		if line.Type == LineEngineFragment {
			engineFragments++
			continue
		}
		seen[line.Type] = true
	}
	for _, required := range []string{LineGoal, LineDone, LineTodo, LineConstraints, LineBackground} {
		if !seen[required] {
			return fmt.Errorf("%w: missing %s", ErrIncompleteSummary, required)
		}
	}
	if engineFragments > 0 {
		return fmt.Errorf("%w: engine_fragment is reserved and must not be produced in phase one", ErrInvalidJSONL)
	}
	return nil
}

func WriteJSONL(dir, taskID, phase string, lines []Line) (string, error) {
	if err := ValidateFiveElements(lines); err != nil {
		return "", err
	}
	raw, err := EncodeJSONL(lines)
	if err != nil {
		return "", err
	}
	targetDir := filepath.Join(dir, taskID, "compaction")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return "", fmt.Errorf("compaction: ensure dir: %w", err)
	}
	name := fmt.Sprintf("%s-%d.jsonl", phase, time.Now().UTC().UnixNano())
	path := filepath.Join(targetDir, name)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", fmt.Errorf("compaction: write jsonl: %w", err)
	}
	return path, nil
}

func GoalHash(goal string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(goal)))
	return "sha256:" + hex.EncodeToString(sum[:])
}
