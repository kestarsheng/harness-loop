package service

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/memoryfile"
)

const (
	DefaultInjectRatio   = 0.15
	DefaultPerItemTokens = 600
	DefaultInjectTopK    = 5
)

type MemoryHit struct {
	TopicKey   string
	Title      string
	Body       string
	Category   string
	SourceFile string
	UpdatedAt  time.Time
	Score      float64
	Pinned     bool
}

type MemoryRecaller interface {
	Recall(ctx context.Context, tenantID, projectID, query string, categories []string, topK int) ([]MemoryHit, error)
}

type InjectedMemory struct {
	TopicKey   string
	SourceFile string
	UpdatedAt  time.Time
	Score      float64
	Body       string
	Tokens     int
	Category   string
}

type InjectResult struct {
	Items      []InjectedMemory
	TokensUsed int
	Ratio      float64
	Degraded   bool
	Categories []string
}

type InjectOptions struct {
	TenantID      string
	ProjectID     string
	Stage         string
	Query         string
	ContextBudget int
	Ratio         float64
	PerItemTokens int
	TopK          int
	Fallback      *MDFallback
}

var stageCategories = map[string][]string{
	"a": {"user_preference", "project_fact"},
	"b": {"long_term_constraint", "project_fact"},
	"c": {"project_fact", "long_term_constraint", "task_state"},
	"d": {"task_state", "project_fact"},
	"e": nil,
}

func CategoriesForStage(stage string) []string {
	cats := stageCategories[stage]
	out := make([]string, len(cats))
	copy(out, cats)
	return out
}

func EstimateTokens(text string) int {
	runes := len([]rune(text))
	if runes == 0 {
		return 0
	}
	return (runes + 1) / 2
}

type MemoryInjector struct {
	recaller MemoryRecaller
	fallback *MDFallback
	events   EventPublisher
}

func NewMemoryInjector(recaller MemoryRecaller, fallback *MDFallback) *MemoryInjector {
	return &MemoryInjector{recaller: recaller, fallback: fallback}
}

func (i *MemoryInjector) WithEvents(p EventPublisher) *MemoryInjector {
	i.events = p
	return i
}

func (i *MemoryInjector) Inject(ctx context.Context, opts InjectOptions) (*InjectResult, error) {
	categories := CategoriesForStage(opts.Stage)
	if len(categories) == 0 {
		return &InjectResult{Categories: categories}, nil
	}
	ratio := opts.Ratio
	if ratio <= 0 {
		ratio = DefaultInjectRatio
	}
	perItem := opts.PerItemTokens
	if perItem <= 0 {
		perItem = DefaultPerItemTokens
	}
	topK := opts.TopK
	if topK <= 0 {
		topK = DefaultInjectTopK
	}

	result := &InjectResult{Categories: categories}
	var hits []MemoryHit
	degraded := false

	if i.recaller != nil {
		recallCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		recalled, err := i.recaller.Recall(recallCtx, opts.TenantID, opts.ProjectID, opts.Query, categories, topK)
		cancel()
		if err == nil {
			hits = recalled
		} else {
			degraded = true
			i.publishDegraded(ctx, opts, err.Error())
		}
	} else {
		degraded = true
		i.publishDegraded(ctx, opts, "memory recaller is not configured")
	}

	fallback := opts.Fallback
	if fallback == nil {
		fallback = i.fallback
	}
	if degraded && fallback != nil {
		if fallbackHits, err := fallback.Recall(ctx, opts, categories, topK); err == nil {
			hits = fallbackHits
		}
	}

	filtered := hits[:0]
	for _, h := range hits {
		if categoryAllowed(h.Category, categories) {
			filtered = append(filtered, h)
		}
	}
	hits = filtered

	budget := int(float64(opts.ContextBudget) * ratio)
	if budget <= 0 {
		budget = perItem
	}
	sort.SliceStable(hits, func(a, b int) bool {
		if hits[a].Score != hits[b].Score {
			return hits[a].Score > hits[b].Score
		}
		return hits[a].TopicKey < hits[b].TopicKey
	})

	used := 0
	for _, hit := range hits {
		if used >= budget {
			break
		}
		remaining := budget - used
		limit := perItem
		if remaining < limit {
			limit = remaining
		}
		body, tokens := truncateToTokens(hit.Body, limit)
		if tokens <= 0 {
			continue
		}
		result.Items = append(result.Items, InjectedMemory{
			TopicKey:   hit.TopicKey,
			SourceFile: hit.SourceFile,
			UpdatedAt:  hit.UpdatedAt,
			Score:      hit.Score,
			Body:       body,
			Tokens:     tokens,
			Category:   hit.Category,
		})
		used += tokens
	}
	result.TokensUsed = used
	result.Degraded = degraded
	if opts.ContextBudget > 0 {
		result.Ratio = float64(used) / float64(opts.ContextBudget)
	}
	return result, nil
}

const truncationSuffix = "…[truncated]"

func truncateToTokens(body string, limit int) (string, int) {
	if limit <= 0 {
		return "", 0
	}
	tokens := EstimateTokens(body)
	if tokens <= limit {
		return body, tokens
	}
	runes := []rune(body)
	suffixTokens := EstimateTokens(truncationSuffix)
	avail := limit - suffixTokens
	if avail <= 0 {
		maxRunes := limit * 2
		if maxRunes > len(runes) {
			maxRunes = len(runes)
		}
		out := string(runes[:maxRunes])
		return out, EstimateTokens(out)
	}
	maxRunes := avail * 2
	if maxRunes > len(runes) {
		maxRunes = len(runes)
	}
	out := string(runes[:maxRunes]) + truncationSuffix
	return out, EstimateTokens(out)
}

func (i *MemoryInjector) publishDegraded(ctx context.Context, opts InjectOptions, reason string) {
	if i.events == nil {
		return
	}
	payload := []byte(fmt.Sprintf(`{"stage":%q,"reason":%q,"fallback":"markdown"}`, opts.Stage, reason))
	if len(payload) > event.MaxPayloadBytes {
		payload = []byte(`{"fallback":"markdown"}`)
	}
	_ = i.events.Publish(ctx, &event.Event{
		EventID:       fmt.Sprintf("degraded-%s-%s-%d", opts.TenantID, opts.Stage, time.Now().UnixNano()),
		EventType:     event.MemoryIndexDegraded,
		SchemaVersion: event.CurrentSchemaVersion,
		TaskID:        opts.ProjectID,
		Stage:         opts.Stage,
		Timestamp:     time.Now().UTC(),
		Severity:      event.SeverityWarn,
		TenantID:      opts.TenantID,
		ProjectID:     opts.ProjectID,
		Payload:       payload,
	})
}

type MDFallback struct {
	GlobalFile  string
	ProjectFile string
}

func (f *MDFallback) Recall(ctx context.Context, opts InjectOptions, categories []string, topK int) ([]MemoryHit, error) {
	var hits []MemoryHit
	for _, path := range []string{f.ProjectFile, f.GlobalFile} {
		if strings.TrimSpace(path) == "" {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		doc, err := memoryfile.Parse(raw)
		if err != nil {
			continue
		}
		for _, topic := range doc.Topics {
			if !categoryAllowed(string(topic.Meta.Category), categories) {
				continue
			}
			score := keywordScore(opts.Query, topic.Title, topic.Body)
			if score <= 0 {
				continue
			}
			if topic.Meta.Pinned {
				score += 1
			}
			hits = append(hits, MemoryHit{
				TopicKey:   topic.Key,
				Title:      topic.Title,
				Body:       topic.Body,
				Category:   string(topic.Meta.Category),
				SourceFile: path,
				UpdatedAt:  topic.Meta.UpdatedAt,
				Score:      score,
				Pinned:     topic.Meta.Pinned,
			})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool {
		if hits[a].Score != hits[b].Score {
			return hits[a].Score > hits[b].Score
		}
		return hits[a].TopicKey < hits[b].TopicKey
	})
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits, nil
}

func categoryAllowed(category string, categories []string) bool {
	for _, c := range categories {
		if c == category {
			return true
		}
	}
	return false
}

func keywordScore(query, title, body string) float64 {
	tokens := strings.Fields(strings.Join(memoryfile.Keywords(query), " "))
	if len(tokens) == 0 {
		return 0
	}
	haystack := strings.ToLower(title + " " + body)
	score := 0.0
	for _, token := range tokens {
		if strings.Contains(haystack, strings.ToLower(token)) {
			score++
		}
	}
	return score
}
