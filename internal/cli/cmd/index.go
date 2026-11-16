package cmd

// `harness-cli index` 是代码索引的本地维护命令：直接连 PostgreSQL 写 hlp_code，
// 不走单文件 gRPC 往返——全量 1000 文件时逐次 RPC 会把吞吐压死。
//
// 三类验收行为都在这里落地：
//  1. --full 全量 + --concurrency/--batch-size 调并发度与批量大小；
//  2. 全量期间读旧索引不阻塞（单文件单事务 + MVCC），可用 --probe-query 现场取证；
//  3. 首次全量可查：每个文件提交即进检索可见集合，--verify-query 在收尾时验一次。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"agent-platform/internal/cli/output"
	"agent-platform/pkg/codeindex"
	"agent-platform/pkg/codeindex/parser"
	"agent-platform/pkg/pgindex"
)

const (
	indexDefaultConcurrency = codeindex.DefaultBatchConcurrency
	indexDefaultBatchSize   = codeindex.DefaultBatchSize
	indexDefaultInsertBatch = codeindex.DefaultInsertBatch
	indexDefaultMaxConns    = 8
	indexDefaultTimeout     = 30 * time.Minute
)

type indexProbeStats struct {
	Probes       int     `json:"probes"`
	OK           int     `json:"ok"`
	Failed       int     `json:"failed"`
	SuccessRate  float64 `json:"success_rate"`
	HitsLast     int     `json:"hits_last"`
	P50LatencyMS int64   `json:"p50_latency_ms"`
	P95LatencyMS int64   `json:"p95_latency_ms"`
	MaxLatencyMS int64   `json:"max_latency_ms"`
	LastError    string  `json:"last_error,omitempty"`
}

type indexVerify struct {
	Query     string `json:"query"`
	Hits      int    `json:"hits"`
	Chunks    int    `json:"chunks"`
	LatencyMS int64  `json:"latency_ms"`
}

type indexPoolJSON struct {
	MaxConns      int32 `json:"max_conns"`
	TotalConns    int32 `json:"total_conns"`
	AcquiredConns int32 `json:"acquired_conns"`
	IdleConns     int32 `json:"idle_conns"`
}

type indexOutput struct {
	OK          bool                `json:"ok"`
	Root        string              `json:"root"`
	TenantID    string              `json:"tenant_id"`
	ProjectID   string              `json:"project_id"`
	Full        bool                `json:"full"`
	Files       int                 `json:"files"`
	Indexed     int                 `json:"indexed"`
	Skipped     int                 `json:"skipped"`
	Failed      int                 `json:"failed"`
	Batches     int                 `json:"batches"`
	Chunks      int                 `json:"chunks"`
	Edges       int                 `json:"edges"`
	Bytes       int64               `json:"bytes"`
	Concurrency int                 `json:"concurrency"`
	BatchSize   int                 `json:"batch_size"`
	InsertBatch int                 `json:"insert_batch"`
	ElapsedMS   int64               `json:"elapsed_ms"`
	P50MS       int64               `json:"p50_ms"`
	P95MS       int64               `json:"p95_ms"`
	P99MS       int64               `json:"p99_ms"`
	MaxMS       int64               `json:"max_ms"`
	Pool        indexPoolJSON       `json:"pool"`
	Probe       *indexProbeStats    `json:"probe,omitempty"`
	Verify      *indexVerify        `json:"verify,omitempty"`
	Failures    []string            `json:"failures,omitempty"`
	Error       *output.ErrorDetail `json:"error,omitempty"`
}

func newIndexCmd() *cobra.Command {
	var (
		root        string
		projectID   string
		full        bool
		concurrency int
		batchSize   int
		insertBatch int
		embedBatch  int
		extensions  []string
		maxFiles    int
		dsn         string
		dbMaxConns  int32
		probeQuery  string
		probeEvery  time.Duration
		verifyQuery string
		timeout     time.Duration
		quiet       bool
	)
	cmd := &cobra.Command{
		Use:   "index",
		Short: "Index a code repository into hlp_code (full/incremental, concurrent)",
		Long: "Walk a repository and write its chunks/edges into hlp_code.\n\n" +
			"--full 强制重写每个文件（全量）；不带 --full 时按 content_hash 跳过未变更文件（增量）。\n" +
			"全量期间检索不受阻塞：每个文件在独立事务里提交，读方在提交前仍看到旧版本。",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(projectID) == "" {
				return indexFailure(cmd, output.ExitInvalidUsage, "E_INVALID_ARGUMENT", "--project-id is required", nil)
			}
			cfg := loadFileConfig(opts.Config)
			tenant := resolveTenant(cfg)
			if strings.TrimSpace(tenant) == "" {
				return indexFailure(cmd, output.ExitInvalidUsage, "E_TENANT_REQUIRED", "tenant is required", nil)
			}
			dsn = resolveIndexDSN(dsn)
			if dsn == "" {
				return indexFailure(cmd, output.ExitInvalidUsage, "E_INVALID_ARGUMENT",
					"missing DSN: pass --dsn or set HARNESS_PG_DSN", nil)
			}

			ctx := cmd.Context()
			if timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}

			pgCfg := pgindex.DefaultConfig(dsn)
			if dbMaxConns > 0 {
				pgCfg.MaxConns = dbMaxConns
			}
			db, err := pgindex.Open(ctx, pgCfg)
			if err != nil {
				return indexFailure(cmd, output.ExitConnectionFail, "E_SERVICE_UNAVAILABLE",
					"open postgres: "+err.Error(), nil)
			}
			defer db.Close()
			if err := db.Migrate(ctx); err != nil {
				return indexFailure(cmd, output.ExitConnectionFail, "E_MIGRATION_FAILED",
					"migrate hlp_code: "+err.Error(), nil)
			}

			registry := parser.NewRegistry()
			writer := codeindex.NewCodeWriter(db, func(path string, src []byte) (codeindex.ParseResult, error) {
				return registry.Parse(path, src), nil
			}, nil, nil)

			batchOpts := codeindex.BatchOptions{
				TenantID:    tenant,
				ProjectID:   projectID,
				Root:        root,
				Concurrency: concurrency,
				BatchSize:   batchSize,
				InsertBatch: insertBatch,
				EmbedBatch:  embedBatch,
				Force:       full,
				Extensions:  extensions,
				MaxFiles:    maxFiles,
			}
			if !quiet && !opts.JSON {
				batchOpts.Progress = func(p codeindex.BatchProgress) {
					fmt.Fprintf(cmd.OutOrStdout(), "[index] %d/%d files batch=%d indexed=%d skipped=%d failed=%d chunks=%d elapsed=%s last=%s\n",
						p.Done, p.Total, p.Batches, p.Indexed, p.Skipped, p.Failed, p.Chunks,
						p.Elapsed.Round(time.Millisecond), p.LastPath)
				}
			}

			// 全量期间并发检索取证：证明批量写不阻塞读旧索引。
			var probe *indexProbeStats
			stopProbe := make(chan struct{})
			probeDone := make(chan *indexProbeStats, 1)
			if strings.TrimSpace(probeQuery) != "" {
				go func() {
					probeDone <- runIndexProbe(ctx, writer, tenant, projectID, probeQuery, probeEvery, stopProbe)
				}()
			} else {
				probeDone <- nil
			}

			stats, indexErr := writer.IndexBatch(ctx, batchOpts)
			close(stopProbe)
			probe = <-probeDone

			result := indexOutput{
				OK:          indexErr == nil && stats.Failed == 0,
				Root:        filepath.ToSlash(mustAbs(root)),
				TenantID:    tenant,
				ProjectID:   projectID,
				Full:        full,
				Files:       stats.Total,
				Indexed:     stats.Indexed,
				Skipped:     stats.Skipped,
				Failed:      stats.Failed,
				Batches:     stats.Batches,
				Chunks:      stats.Chunks,
				Edges:       stats.Edges,
				Bytes:       stats.Bytes,
				Concurrency: stats.Concurrency,
				BatchSize:   stats.BatchSize,
				InsertBatch: insertBatch,
				ElapsedMS:   stats.Elapsed.Milliseconds(),
				P50MS:       stats.P50.Milliseconds(),
				P95MS:       stats.P95.Milliseconds(),
				P99MS:       stats.P99.Milliseconds(),
				MaxMS:       stats.Max.Milliseconds(),
				Pool:        toIndexPool(writer.PoolStatus()),
				Probe:       probe,
			}
			for _, failure := range stats.Failures {
				result.Failures = append(result.Failures, output.Redact(failure.Path+": "+failure.Err.Error()))
			}

			if strings.TrimSpace(verifyQuery) != "" {
				hits, searchStats, searchErr := writer.Search(ctx, codeindex.SearchOptions{
					TenantID: tenant, ProjectID: projectID, Query: verifyQuery, TopK: 5,
				})
				if searchErr != nil {
					result.Verify = &indexVerify{Query: verifyQuery, LatencyMS: searchStats.LatencyMs}
					result.OK = false
				} else {
					chunks := 0
					for _, hit := range hits {
						if hit.ChunkID != "" {
							chunks++
						}
					}
					result.Verify = &indexVerify{
						Query: verifyQuery, Hits: chunks, Chunks: searchStats.Merged, LatencyMS: searchStats.LatencyMs,
					}
					if chunks == 0 {
						result.OK = false
					}
				}
			}

			if opts.JSON {
				if err := emitJSON(cmd, result); err != nil {
					return err
				}
			} else {
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "%s\n", stats.Summary())
				fmt.Fprintf(out, "pool max=%d acquired=%d idle=%d\n",
					result.Pool.MaxConns, result.Pool.AcquiredConns, result.Pool.IdleConns)
				if probe != nil {
					fmt.Fprintf(out, "probe probes=%d ok=%d failed=%d success=%.2f%% p95=%dms max=%dms\n",
						probe.Probes, probe.OK, probe.Failed, probe.SuccessRate, probe.P95LatencyMS, probe.MaxLatencyMS)
				}
				if result.Verify != nil {
					fmt.Fprintf(out, "verify query=%q hits=%d latency=%dms\n",
						result.Verify.Query, result.Verify.Hits, result.Verify.LatencyMS)
				}
				if len(result.Failures) > 0 {
					fmt.Fprintf(out, "failures=%d\n", len(result.Failures))
					for i, failure := range result.Failures {
						if i >= 10 {
							fmt.Fprintf(out, "  ... %d more\n", len(result.Failures)-10)
							break
						}
						fmt.Fprintf(out, "  %s\n", failure)
					}
				}
			}

			switch {
			case indexErr != nil && errors.Is(indexErr, context.Canceled):
				return &ExitError{Code: output.ExitAborted}
			case indexErr != nil:
				return indexFailure(cmd, output.ExitFailed, "E_INDEX_FAILED", indexErr.Error(), nil)
			case !result.OK:
				return &ExitError{Code: output.ExitFailed}
			default:
				return nil
			}
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&root, "path", ".", "repository root to index")
	flags.StringVar(&projectID, "project-id", "", "project id for hlp_code rows (required)")
	flags.BoolVar(&full, "full", false, "full reindex: rewrite every file, ignore content_hash skips")
	flags.IntVar(&concurrency, "concurrency", indexDefaultConcurrency, "number of concurrent indexing workers")
	flags.IntVar(&batchSize, "batch-size", indexDefaultBatchSize, "files per batch (progress/checkpoint granularity)")
	flags.IntVar(&insertBatch, "insert-batch", indexDefaultInsertBatch, "rows per multi-row INSERT")
	flags.IntVar(&embedBatch, "embed-batch", 0, "texts per embedding call (0 = writer default)")
	flags.StringSliceVar(&extensions, "ext", nil, "only index these extensions, e.g. --ext .go --ext .ts")
	flags.IntVar(&maxFiles, "max-files", 0, "cap the number of files (0 = no cap)")
	flags.StringVar(&dsn, "dsn", "", "PostgreSQL DSN (default $HARNESS_PG_DSN)")
	flags.Int32Var(&dbMaxConns, "db-max-conns", indexDefaultMaxConns, "connection pool size; concurrency is capped to max-1 to keep reads unblocked")
	flags.StringVar(&probeQuery, "probe-query", "", "run this search repeatedly during the batch to prove reads stay unblocked")
	flags.DurationVar(&probeEvery, "probe-interval", 500*time.Millisecond, "interval between probe searches")
	flags.StringVar(&verifyQuery, "verify-query", "", "run this search after indexing and require at least one hit")
	flags.DurationVar(&timeout, "timeout", indexDefaultTimeout, "overall timeout (0 = none)")
	flags.BoolVar(&quiet, "quiet", false, "suppress per-batch progress lines")
	return cmd
}

// resolveIndexDSN 依次取 --dsn、HARNESS_PG_DSN、HARNESS_TEST_PG_DSN（压测环境复用测试库）。
func resolveIndexDSN(flag string) string {
	if strings.TrimSpace(flag) != "" {
		return strings.TrimSpace(flag)
	}
	for _, key := range []string{"HARNESS_PG_DSN", "HARNESS_TEST_PG_DSN"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

// runIndexProbe 在批量索引期间反复检索，统计成功率与延迟分布。
// 检索失败（拿不到连接、锁等待超时）都会如实计进 failed。
func runIndexProbe(ctx context.Context, writer *codeindex.CodeWriter, tenantID, projectID, query string,
	every time.Duration, stop <-chan struct{}) *indexProbeStats {
	if every <= 0 {
		every = 500 * time.Millisecond
	}
	stats := &indexProbeStats{}
	var latencies []time.Duration
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			closeProbe(stats, latencies)
			return stats
		case <-ctx.Done():
			closeProbe(stats, latencies)
			return stats
		case <-ticker.C:
			probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			hits, searchStats, err := writer.Search(probeCtx, codeindex.SearchOptions{
				TenantID: tenantID, ProjectID: projectID, Query: query, TopK: 5,
			})
			cancel()
			stats.Probes++
			if err != nil {
				stats.Failed++
				stats.LastError = output.Redact(err.Error())
				continue
			}
			stats.OK++
			stats.HitsLast = len(hits)
			latencies = append(latencies, time.Duration(searchStats.LatencyMs)*time.Millisecond)
		}
	}
}

func closeProbe(stats *indexProbeStats, latencies []time.Duration) {
	if stats.Probes > 0 {
		stats.SuccessRate = float64(stats.OK) / float64(stats.Probes) * 100
	}
	if len(latencies) == 0 {
		return
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	stats.P50LatencyMS = latencies[len(latencies)/2].Milliseconds()
	stats.P95LatencyMS = codeindex.Percentile(latencies, 0.95).Milliseconds()
	stats.MaxLatencyMS = latencies[len(latencies)-1].Milliseconds()
}

func toIndexPool(status codeindex.PoolStatus) indexPoolJSON {
	return indexPoolJSON{
		MaxConns:      status.MaxConns,
		TotalConns:    status.TotalConns,
		AcquiredConns: status.AcquiredConns,
		IdleConns:     status.IdleConns,
	}
}

func mustAbs(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

func indexFailure(cmd *cobra.Command, code int, errCode, message string, details map[string]any) error {
	env := output.Failure(errCode, message, details)
	if opts.JSON {
		if err := emitJSON(cmd, env); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "ERROR %s: %s\n", errCode, output.Redact(message))
	}
	return &ExitError{Code: code}
}
