package pgindex

import (
	"context"
	"fmt"
	"strings"
	"time"

	"agent-platform/pkg/rrf"
)

type RecallOptions struct {
	TenantID   string
	ProjectID  string
	Scope      string
	Categories []string
	Query      string
	TopK       int
	RRFK       int
	Semantic   bool
	Embedder   func(ctx context.Context, text string) ([]float32, error)
}

type RecallHit struct {
	TopicRecord
	RRFScore            float64
	Sources             []string
	Ranks               []int
	VectorTriggerReason string
}

type RecallStats struct {
	FullTextHits int
	VectorHits   int
	Merged       int
	VectorCalled bool
	Trigger      string
	LatencyMs    int64
}

const (
	TriggerInsufficient = "insufficient"
	TriggerSemantic     = "semantic"
)

var semanticMarkers = []string{"如何", "为什么", "怎么", "类似", "相关", "similar", "how", "why", "explain"}

func needsVector(query string, fulltextHits, topK int) (bool, string) {
	tokens := strings.Fields(Tokenize(query))
	lower := strings.ToLower(query)
	for _, marker := range semanticMarkers {
		if strings.Contains(lower, marker) {
			return true, TriggerSemantic
		}
	}
	if len(tokens) > 8 {
		return true, TriggerSemantic
	}
	if fulltextHits < topK {
		return true, TriggerInsufficient
	}
	return false, ""
}

func (w *MemoryWriter) Recall(ctx context.Context, opts RecallOptions) ([]RecallHit, RecallStats, error) {
	start := time.Now()
	stats := RecallStats{}
	topK := opts.TopK
	if topK <= 0 || topK > 50 {
		topK = 10
	}
	scope := opts.Scope
	categories := opts.Categories

	fulltext, err := w.searchTopicsFiltered(ctx, opts.TenantID, opts.ProjectID, opts.Query, topK*2, scope, categories)
	if err != nil {
		return nil, stats, err
	}
	stats.FullTextHits = len(fulltext)

	need, reason := needsVector(opts.Query, len(fulltext), topK)
	if opts.Semantic {
		need, reason = true, TriggerSemantic
	}
	stats.Trigger = reason

	var vectorHits []TopicRecord
	if need && opts.Embedder != nil {
		stats.VectorCalled = true
		vec, embedErr := opts.Embedder(ctx, opts.Query)
		if embedErr == nil && len(vec) > 0 {
			vectorHits, err = w.vectorSearch(ctx, opts.TenantID, opts.ProjectID, vec, topK*2, scope, categories)
			if err != nil {
				vectorHits = nil
			}
		}
	}
	stats.VectorHits = len(vectorHits)

	left := routeOf("fulltext", fulltext)
	right := routeOf("vector", vectorHits)
	merged := rrf.New(opts.RRFK).MergeRoutes(left, right)

	hits := make([]RecallHit, 0, len(merged))
	byKey := make(map[string]TopicRecord, len(fulltext)+len(vectorHits))
	for _, rec := range fulltext {
		byKey[rec.TopicKey] = rec
	}
	for _, rec := range vectorHits {
		byKey[rec.TopicKey] = rec
	}
	for _, h := range merged {
		if len(hits) >= topK {
			break
		}
		rec, ok := byKey[h.Entity.ID]
		if !ok {
			continue
		}
		hits = append(hits, RecallHit{
			TopicRecord:         rec,
			RRFScore:            h.RRFScore,
			Sources:             h.Sources,
			Ranks:               h.Ranks,
			VectorTriggerReason: reason,
		})
	}
	stats.Merged = len(hits)
	stats.LatencyMs = time.Since(start).Milliseconds()
	return hits, stats, nil
}

func routeOf(name string, records []TopicRecord) rrf.Route {
	items := make([]rrf.RankedItem, 0, len(records))
	for _, rec := range records {
		items = append(items, rrf.RankedItem{
			Entity: rrf.Entity{ID: rec.TopicKey, FilePath: rec.SourceFile},
		})
	}
	return rrf.Route{Name: name, Items: items}
}

func stripSemanticMarkers(query string) string {
	lowered := strings.ToLower(query)
	for _, marker := range semanticMarkers {
		lowered = strings.ReplaceAll(lowered, marker, " ")
	}
	return lowered
}

func toOrTsQuery(query string) string {
	tokens := strings.Fields(TokensForTsv(stripSemanticMarkers(query)))
	if len(tokens) == 0 {
		return ""
	}
	return strings.Join(tokens, " | ")
}

func (w *MemoryWriter) searchTopicsFiltered(ctx context.Context, tenantID, projectID, query string, limit int, scope string, categories []string) ([]TopicRecord, error) {
	tsQuery := toOrTsQuery(query)
	rows, err := w.db.Query(ctx, `
		SELECT tenant_id, project_id, scope, topic_key, category, title, body, pinned,
		       order_rank, content_hash, source_file, updated_at, decayed
		FROM hlp_mem.memory_topics
		WHERE tenant_id = $1 AND project_id = $2 AND decayed = false
		  AND ($3 = '' OR scope = $3)
		  AND (COALESCE(cardinality($4::text[]), 0) = 0 OR category = ANY($4))
		  AND ($5 = '' OR tsv @@ to_tsquery('simple', $5))
		ORDER BY ts_rank_cd(tsv, to_tsquery('simple', $5)) DESC, order_rank ASC
		LIMIT $6`,
		tenantID, projectID, scope, categories, tsQuery, limit)
	if err != nil {
		return nil, fmt.Errorf("recall: fulltext: %w", err)
	}
	defer rows.Close()
	return scanTopicRows(rows)
}

func (w *MemoryWriter) vectorSearch(ctx context.Context, tenantID, projectID string, vec []float32, limit int, scope string, categories []string) ([]TopicRecord, error) {
	literal := vectorLiteral(vec)
	rows, err := w.db.Query(ctx, `
		SELECT t.tenant_id, t.project_id, t.scope, t.topic_key, t.category, t.title, t.body, t.pinned,
		       t.order_rank, t.content_hash, t.source_file, t.updated_at, t.decayed
		FROM hlp_mem.memory_topics t
		JOIN hlp_mem.memory_chunks c ON c.topic_id = t.topic_id
		WHERE t.tenant_id = $2 AND t.project_id = $3 AND t.decayed = false
		  AND ($4 = '' OR t.scope = $4)
		  AND (COALESCE(cardinality($5::text[]), 0) = 0 OR t.category = ANY($5))
		  AND c.embedding IS NOT NULL
		ORDER BY c.embedding <=> $1::vector
		LIMIT $6`,
		literal, tenantID, projectID, scope, categories, limit)
	if err != nil {
		return nil, fmt.Errorf("recall: vector: %w", err)
	}
	defer rows.Close()
	return scanTopicRows(rows)
}

func vectorLiteral(vec []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range vec {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(fmt.Sprintf("%.6f", v))
	}
	b.WriteByte(']')
	return b.String()
}

func (w *MemoryWriter) SetEmbedding(ctx context.Context, tenantID, projectID, scope, topicKey string, vec []float32) error {
	_, err := w.db.Exec(ctx, `
		UPDATE hlp_mem.memory_chunks c
		SET embedding = $1::vector
		FROM hlp_mem.memory_topics t
		WHERE c.topic_id = t.topic_id
		  AND t.tenant_id = $2 AND t.project_id = $3 AND t.scope = $4 AND t.topic_key = $5`,
		vectorLiteral(vec), tenantID, projectID, scope, topicKey)
	if err != nil {
		return fmt.Errorf("recall: set embedding %s: %w", topicKey, err)
	}
	return nil
}
