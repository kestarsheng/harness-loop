package compaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agent-platform/pkg/llm"
)

type TemplateVersion string

const TemplateV1 TemplateVersion = "template.v1"

var ErrUnknownTemplate = errors.New("compaction: unknown template version")
var ErrIncompleteSummary = errors.New("compaction: summary is missing one of the five elements")

type Message struct {
	Role    string
	Content string
}

type History struct {
	Goal        string
	GoalHash    string
	Phase       string
	Messages    []Message
	Done        []string
	Todo        []string
	Constraints []string
	Background  []string
}

type Summary struct {
	Goal        string
	Done        string
	Todo        string
	Constraints string
	Background  string
	SourceRefs  map[string][]string
}

type Summarizer interface {
	Summarize(ctx context.Context, h History, tmpl TemplateVersion) (Summary, error)
}

type Completer interface {
	Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

type LLMCompleter struct {
	Client llm.Client
	Model  string
}

func (c LLMCompleter) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	if c.Client == nil {
		return "", errors.New("compaction: llm client is nil")
	}
	resp, err := c.Client.Chat(ctx, &llm.ChatRequest{
		Messages:     []llm.Message{{Role: "user", Content: userPrompt}},
		Model:        c.Model,
		Temperature:  0,
		SystemPrompt: systemPrompt,
	})
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

type TemplateSummarizer struct {
	completer Completer
}

func NewTemplateSummarizer(completer Completer) *TemplateSummarizer {
	return &TemplateSummarizer{completer: completer}
}

func (s *TemplateSummarizer) Summarize(ctx context.Context, h History, tmpl TemplateVersion) (Summary, error) {
	system, user, err := BuildPrompt(tmpl, h)
	if err != nil {
		return Summary{}, err
	}
	if s.completer == nil {
		return Summary{}, errors.New("compaction: completer is nil")
	}
	raw, err := s.completer.Complete(ctx, system, user)
	if err != nil {
		return Summary{}, fmt.Errorf("compaction: summarize: %w", err)
	}
	summary, err := ParseSummary(raw)
	if err != nil {
		return Summary{}, err
	}
	summary.SourceRefs = map[string][]string{
		"goal":        {"task:goal"},
		"done":        {"event:stage"},
		"todo":        {"plan:current"},
		"constraints": {"memory:constraint"},
		"background":  {"context:history"},
	}
	return summary, nil
}

const fiveElementsInstruction = `你是上下文压缩器。只输出一个 JSON 对象，且必须包含且仅包含以下五个字符串字段：
- goal：任务原始目标（逐字保留，不得改写）
- done：已完成事项的确定性汇总
- todo：未完成事项与当前阶段目标
- constraints：必须遵守的长期约束
- background：关键背景（可以是"无新增背景"）
禁止编造事实，禁止输出 JSON 以外的任何内容。`

func BuildPrompt(tmpl TemplateVersion, h History) (system, user string, err error) {
	switch tmpl {
	case TemplateV1, "":
	default:
		return "", "", fmt.Errorf("%w: %s", ErrUnknownTemplate, tmpl)
	}
	var b strings.Builder
	b.WriteString("阶段: ")
	b.WriteString(h.Phase)
	b.WriteString("\n原始目标: ")
	b.WriteString(h.Goal)
	if h.GoalHash != "" {
		b.WriteString("\n目标哈希: ")
		b.WriteString(h.GoalHash)
	}
	if len(h.Done) > 0 {
		b.WriteString("\n已完成:\n- ")
		b.WriteString(strings.Join(h.Done, "\n- "))
	}
	if len(h.Todo) > 0 {
		b.WriteString("\n未完成:\n- ")
		b.WriteString(strings.Join(h.Todo, "\n- "))
	}
	if len(h.Constraints) > 0 {
		b.WriteString("\n约束:\n- ")
		b.WriteString(strings.Join(h.Constraints, "\n- "))
	}
	if len(h.Background) > 0 {
		b.WriteString("\n背景:\n- ")
		b.WriteString(strings.Join(h.Background, "\n- "))
	}
	if len(h.Messages) > 0 {
		b.WriteString("\n最近消息:")
		start := 0
		if len(h.Messages) > 20 {
			start = len(h.Messages) - 20
		}
		for _, m := range h.Messages[start:] {
			b.WriteString("\n[")
			b.WriteString(m.Role)
			b.WriteString("] ")
			b.WriteString(m.Content)
		}
	}
	return fiveElementsInstruction, b.String(), nil
}

var requiredSummaryFields = []string{"goal", "done", "todo", "constraints", "background"}

func ParseSummary(raw string) (Summary, error) {
	trimmed := strings.TrimSpace(raw)
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start < 0 || end <= start {
		return Summary{}, fmt.Errorf("%w: output is not a json object", ErrIncompleteSummary)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(trimmed[start:end+1]), &fields); err != nil {
		return Summary{}, fmt.Errorf("%w: invalid json: %v", ErrIncompleteSummary, err)
	}
	for _, name := range requiredSummaryFields {
		value, ok := fields[name]
		if !ok {
			return Summary{}, fmt.Errorf("%w: missing %s", ErrIncompleteSummary, name)
		}
		if _, isString := value.(string); !isString {
			return Summary{}, fmt.Errorf("%w: %s must be a string", ErrIncompleteSummary, name)
		}
	}
	summary := Summary{
		Goal:        fields["goal"].(string),
		Done:        fields["done"].(string),
		Todo:        fields["todo"].(string),
		Constraints: fields["constraints"].(string),
		Background:  fields["background"].(string),
	}
	if strings.TrimSpace(summary.Goal) == "" || strings.TrimSpace(summary.Done) == "" ||
		strings.TrimSpace(summary.Todo) == "" || strings.TrimSpace(summary.Constraints) == "" {
		return Summary{}, fmt.Errorf("%w: goal/done/todo/constraints must be non-empty", ErrIncompleteSummary)
	}
	return summary, nil
}
