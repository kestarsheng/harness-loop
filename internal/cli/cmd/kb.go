package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"agent-platform/internal/cli/output"
	"agent-platform/internal/cli/tui"
	knowledgepb "agent-platform/pkg/pb/knowledge"
)

const kbTitleLookupLimit = 8

type kbResult struct {
	Rank       int
	Title      string
	Snippet    string
	Source     string
	DocumentID string
	ChunkID    string
	Score      float64
}

type kbSearchResult struct {
	Results   []kbResult
	Total     int64
	LatencyMS int64
}

func newKBCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kb",
		Short: "Search, upload and inspect knowledge-base documents",
	}
	cmd.AddCommand(newKBSearchCmd(), newKBUploadCmd(), newKBShowCmd())
	return cmd
}

func newKBSearchCmd() *cobra.Command {
	var (
		query     string
		topK      int
		searchTyp string
		kbID      string
		threshold float64
	)
	cmd := &cobra.Command{
		Use:   "search",
		Short: "Search the knowledge base and print sources with scores",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(query) == "" {
				return fmt.Errorf("--query is required")
			}
			client, conn, tenant, err := newKnowledgeClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()
			result, err := searchKnowledge(callContext(cmd, tenant), client, tenant, knowledgeSearchOptions{
				Query: query, TopK: topK, Type: searchTyp, KBID: kbID, Threshold: threshold,
			})
			if err != nil {
				return kbFailure(cmd, "kb search", err)
			}
			if opts.JSON {
				return emitJSON(cmd, kbSearchOutput{
					OK:        true,
					Query:     query,
					Results:   toKBSearchJSON(result.Results),
					Total:     result.Total,
					LatencyMS: result.LatencyMS,
				})
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "query=%q results=%d latency=%dms\n", query, result.Total, result.LatencyMS)
			if len(result.Results) == 0 {
				fmt.Fprintln(out, "no results")
				return nil
			}
			for _, item := range result.Results {
				fmt.Fprintf(out, "%d. score=%.4f title=%s\n", item.Rank, item.Score, displayKB(item.Title))
				fmt.Fprintf(out, "   source=%s chunk=%s\n", displayKB(item.Source), displayKB(item.ChunkID))
				if item.Snippet != "" {
					fmt.Fprintf(out, "   snippet=%s\n", singleLine(item.Snippet))
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&query, "query", "", "search query")
	cmd.Flags().IntVar(&topK, "top-k", 10, "maximum number of results")
	cmd.Flags().StringVar(&searchTyp, "type", "hybrid", "search type: vector, bm25 or hybrid")
	cmd.Flags().StringVar(&kbID, "kb", "", "knowledge base id")
	cmd.Flags().Float64Var(&threshold, "score-threshold", 0, "minimum vector score")
	return cmd
}

func newKBUploadCmd() *cobra.Command {
	var (
		file        string
		kbID        string
		strategy    string
		chunkSize   int
		overlap     int
		waitTimeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "upload",
		Short: "Upload a document and follow its pending/processing/ready/failed states",
		RunE: func(cmd *cobra.Command, args []string) error {
			if file == "" {
				return fmt.Errorf("--file is required")
			}
			raw, err := os.ReadFile(file)
			if err != nil {
				return fmt.Errorf("read file: %w", err)
			}
			client, conn, tenant, err := newKnowledgeClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()
			ctx := callContext(cmd, tenant)
			started := time.Now()
			states := []kbState{{Status: "pending", AtMS: started.UnixMilli()}}
			resp, err := client.Upload(ctx, &knowledgepb.UploadRequest{
				Filename:      baseName(file),
				Content:       raw,
				ChunkStrategy: strategy,
				ChunkSize:     int32(chunkSize),
				ChunkOverlap:  int32(overlap),
				TenantId:      tenant,
				KbId:          kbID,
			})
			if err != nil {
				return kbFailure(cmd, "kb upload", err)
			}
			status := normalizeDocStatus(resp.GetStatus())
			states = appendState(states, status)
			docID := resp.GetDocumentId()
			if !opts.JSON {
				fmt.Fprintf(cmd.OutOrStdout(), "document=%s chunk_count=%d status=%s\n", docID, resp.GetChunkCount(), status)
			}
			deadline := time.Now().Add(waitTimeout)
			for status != "ready" && status != "failed" && time.Now().Before(deadline) {
				time.Sleep(300 * time.Millisecond)
				doc, getErr := client.GetDocument(ctx, &knowledgepb.GetDocumentRequest{Id: docID, TenantId: tenant})
				if getErr != nil {
					continue
				}
				next := normalizeDocStatus(doc.GetStatus())
				if next != status {
					status = next
					states = appendState(states, status)
					if !opts.JSON {
						fmt.Fprintf(cmd.OutOrStdout(), "document=%s status=%s\n", docID, status)
					}
				}
			}
			if opts.JSON {
				if err := emitJSON(cmd, kbUploadOutput{
					OK:         status == "ready",
					DocumentID: docID,
					Filename:   baseName(file),
					ChunkCount: resp.GetChunkCount(),
					Status:     status,
					States:     states,
				}); err != nil {
					return err
				}
			}
			switch status {
			case "ready":
				return nil
			case "failed":
				return &ExitError{Code: output.ExitFailed}
			default:
				if !opts.JSON {
					fmt.Fprintf(cmd.OutOrStdout(), "document=%s did not reach ready within %s (last status=%s)\n", docID, waitTimeout, status)
				}
				return &ExitError{Code: output.ExitConnectionFail}
			}
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "document to upload")
	cmd.Flags().StringVar(&kbID, "kb", "", "knowledge base id")
	cmd.Flags().StringVar(&strategy, "chunk-strategy", "", "chunk strategy (token/fixed)")
	cmd.Flags().IntVar(&chunkSize, "chunk-size", 512, "chunk size")
	cmd.Flags().IntVar(&overlap, "chunk-overlap", 50, "chunk overlap")
	cmd.Flags().DurationVar(&waitTimeout, "timeout", 30*time.Second, "max wait for a terminal parse state")
	return cmd
}

func newKBShowCmd() *cobra.Command {
	var (
		id    string
		full  bool
		limit int
	)
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show a knowledge document and its parse status",
		RunE: func(cmd *cobra.Command, args []string) error {
			if id == "" {
				return fmt.Errorf("--id is required")
			}
			client, conn, tenant, err := newKnowledgeClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()
			doc, err := client.GetDocument(callContext(cmd, tenant), &knowledgepb.GetDocumentRequest{Id: id, TenantId: tenant})
			if err != nil {
				return kbFailure(cmd, "kb show", err)
			}
			if opts.JSON {
				return emitJSON(cmd, kbShowOutput{OK: true, Document: kbDocumentJSON{
					ID:         doc.GetId(),
					Filename:   doc.GetFilename(),
					Title:      doc.GetTitle(),
					Status:     normalizeDocStatus(doc.GetStatus()),
					ChunkCount: doc.GetChunkCount(),
					CreatedAt:  doc.GetCreatedAt(),
					UpdatedAt:  doc.GetUpdatedAt(),
					Content:    doc.GetContent(),
				}})
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "id=%s status=%s title=%s filename=%s chunks=%d\n",
				doc.GetId(), normalizeDocStatus(doc.GetStatus()), displayKB(doc.GetTitle()), displayKB(doc.GetFilename()), doc.GetChunkCount())
			fmt.Fprintf(out, "created=%s updated=%s\n",
				formatUnix(doc.GetCreatedAt()), formatUnix(doc.GetUpdatedAt()))
			content := doc.GetContent()
			if !full {
				content = truncateRunes(content, limit)
			}
			if strings.TrimSpace(content) == "" {
				fmt.Fprintln(out, "content=(empty)")
				return nil
			}
			fmt.Fprintln(out, "content=")
			fmt.Fprintln(out, content)
			return nil
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "document id")
	cmd.Flags().BoolVar(&full, "full", false, "print the full document content")
	cmd.Flags().IntVar(&limit, "limit", 400, "content preview length in runes")
	return cmd
}

type knowledgeSearchOptions struct {
	Query     string
	TopK      int
	Type      string
	KBID      string
	Threshold float64
}

func searchKnowledge(ctx context.Context, client knowledgepb.KnowledgeServiceClient, tenant string, req knowledgeSearchOptions) (kbSearchResult, error) {
	if req.TopK <= 0 {
		req.TopK = 10
	}
	resp, err := client.Search(ctx, &knowledgepb.SearchRequest{
		Query:          req.Query,
		TopK:           int32(req.TopK),
		SearchType:     req.Type,
		ScoreThreshold: req.Threshold,
		TenantId:       tenant,
		KbId:           req.KBID,
	})
	if err != nil {
		return kbSearchResult{}, err
	}
	rows := make([]kbResult, 0, len(resp.GetResults()))
	docCache := map[string]*knowledgepb.Document{}
	for i, item := range resp.GetResults() {
		row := kbResult{
			Rank:       i + 1,
			DocumentID: item.GetDocumentId(),
			ChunkID:    item.GetChunkId(),
			Score:      item.GetScore(),
			Snippet:    item.GetContent(),
		}
		if len(item.GetSources()) > 0 {
			source := item.GetSources()[0]
			row.Title = source.GetDocumentName()
			row.Source = source.GetUri()
			if source.GetSnippet() != "" {
				row.Snippet = source.GetSnippet()
			}
			if row.Source == "" {
				row.Source = source.GetDocumentName()
			}
			if source.GetSection() != "" {
				row.Source += "#" + source.GetSection()
			}
		}
		if row.Title == "" && i < kbTitleLookupLimit && row.DocumentID != "" {
			doc, ok := docCache[row.DocumentID]
			if !ok {
				loaded, getErr := client.GetDocument(ctx, &knowledgepb.GetDocumentRequest{Id: row.DocumentID, TenantId: tenant})
				if getErr == nil && loaded != nil {
					doc = loaded
					docCache[row.DocumentID] = loaded
				}
			}
			if doc != nil {
				row.Title = doc.GetTitle()
				if row.Title == "" {
					row.Title = doc.GetFilename()
				}
				if row.Source == "" {
					row.Source = doc.GetFilename()
				}
			}
		}
		if row.Title == "" {
			row.Title = row.DocumentID
		}
		if row.Source == "" {
			row.Source = row.DocumentID
		}
		row.Snippet = singleLine(row.Snippet)
		rows = append(rows, row)
	}
	return kbSearchResult{Results: rows, Total: resp.GetTotal(), LatencyMS: resp.GetLatencyMs()}, nil
}

type kbSearchOutput struct {
	OK        bool                `json:"ok"`
	Query     string              `json:"query"`
	Results   []kbResultJSON      `json:"results"`
	Total     int64               `json:"total"`
	LatencyMS int64               `json:"latency_ms"`
	Error     *output.ErrorDetail `json:"error,omitempty"`
}

type kbResultJSON struct {
	Rank       int     `json:"rank"`
	Title      string  `json:"title"`
	Snippet    string  `json:"snippet"`
	Source     string  `json:"source"`
	DocumentID string  `json:"document_id"`
	ChunkID    string  `json:"chunk_id"`
	Score      float64 `json:"score"`
}

type kbState struct {
	Status string `json:"status"`
	AtMS   int64  `json:"at_ms"`
}

type kbUploadOutput struct {
	OK         bool                `json:"ok"`
	DocumentID string              `json:"document_id"`
	Filename   string              `json:"filename"`
	ChunkCount int32               `json:"chunk_count"`
	Status     string              `json:"status"`
	States     []kbState           `json:"states"`
	Error      *output.ErrorDetail `json:"error,omitempty"`
}

type kbShowOutput struct {
	OK       bool                `json:"ok"`
	Document kbDocumentJSON      `json:"document"`
	Error    *output.ErrorDetail `json:"error,omitempty"`
}

type kbDocumentJSON struct {
	ID         string `json:"id"`
	Filename   string `json:"filename"`
	Title      string `json:"title"`
	Status     string `json:"status"`
	ChunkCount int32  `json:"chunk_count"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
	Content    string `json:"content,omitempty"`
}

func toKBSearchJSON(items []kbResult) []kbResultJSON {
	out := make([]kbResultJSON, 0, len(items))
	for _, item := range items {
		out = append(out, kbResultJSON{
			Rank:       item.Rank,
			Title:      item.Title,
			Snippet:    item.Snippet,
			Source:     item.Source,
			DocumentID: item.DocumentID,
			ChunkID:    item.ChunkID,
			Score:      item.Score,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Rank < out[j].Rank })
	return out
}

func newKnowledgeClient(cmd *cobra.Command) (knowledgepb.KnowledgeServiceClient, *grpc.ClientConn, string, error) {
	cfg := loadFileConfig(opts.Config)
	server := resolveKBServer(cfg)
	tenant := resolveTenant(cfg)
	ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, server, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		return nil, nil, "", &ExitError{Code: output.ExitConnectionFail}
	}
	return knowledgepb.NewKnowledgeServiceClient(conn), conn, tenant, nil
}

type knowledgeTUISource struct {
	client knowledgepb.KnowledgeServiceClient
	tenant string
}

func (s *knowledgeTUISource) Search(ctx context.Context, query string, topK int) ([]tui.KBSearchResult, int64, time.Duration, error) {
	started := time.Now()
	result, err := searchKnowledge(ctx, s.client, s.tenant, knowledgeSearchOptions{Query: query, TopK: topK, Type: "hybrid"})
	if err != nil {
		return nil, 0, 0, err
	}
	out := make([]tui.KBSearchResult, 0, len(result.Results))
	for _, item := range result.Results {
		out = append(out, tui.KBSearchResult{
			Title:      item.Title,
			Snippet:    item.Snippet,
			Source:     item.Source,
			DocumentID: item.DocumentID,
			ChunkID:    item.ChunkID,
			Score:      item.Score,
		})
	}
	latency := time.Duration(result.LatencyMS) * time.Millisecond
	if latency == 0 {
		latency = time.Since(started)
	}
	return out, result.Total, latency, nil
}

func newTUIKBSource(cmd *cobra.Command) (tui.KBSource, *grpc.ClientConn) {
	cfg := loadFileConfig(opts.Config)
	server := resolveKBServer(cfg)
	tenant := resolveTenant(cfg)
	conn, err := grpc.DialContext(cmd.Context(), server, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil || conn == nil {
		return nil, nil
	}
	return &knowledgeTUISource{client: knowledgepb.NewKnowledgeServiceClient(conn), tenant: tenant}, conn
}

func kbFailure(cmd *cobra.Command, action string, err error) error {
	message := action + ": " + err.Error()
	if opts.JSON {
		if emitErr := emitJSON(cmd, kbFailureOutput{OK: false, Error: &output.ErrorDetail{
			Code:    describeError(err).Error.Code,
			Message: output.Redact(message),
		}}); emitErr != nil {
			return emitErr
		}
		return &ExitError{Code: exitCodeForError(err)}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "ERROR %s: %s\n", describeError(err).Error.Code, output.Redact(message))
	return &ExitError{Code: exitCodeForError(err)}
}

type kbFailureOutput struct {
	OK    bool                `json:"ok"`
	Error *output.ErrorDetail `json:"error"`
}

func emitJSON(cmd *cobra.Command, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fmt.Fprintln(cmd.OutOrStdout(), output.Redact(err.Error()))
		return nil
	}
	fmt.Fprintln(cmd.OutOrStdout(), output.Redact(string(raw)))
	return nil
}

func appendState(states []kbState, status string) []kbState {
	if len(states) > 0 && states[len(states)-1].Status == status {
		return states
	}
	return append(states, kbState{Status: status, AtMS: time.Now().UnixMilli()})
}

func normalizeDocStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ready", "failed", "pending", "processing":
		return strings.ToLower(strings.TrimSpace(status))
	case "":
		return "pending"
	default:
		return strings.ToLower(strings.TrimSpace(status))
	}
}

func displayKB(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return singleLine(value)
}

func singleLine(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.TrimSpace(value)
}

func truncateRunes(value string, max int) string {
	runes := []rune(value)
	if max <= 0 || len(runes) <= max {
		return value
	}
	return string(runes[:max]) + "…"
}

func baseName(path string) string {
	path = strings.ReplaceAll(path, "\\", "/")
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[idx+1:]
	}
	return path
}

func formatUnix(seconds int64) string {
	if seconds <= 0 {
		return "-"
	}
	return time.Unix(seconds, 0).Format(time.RFC3339)
}
