package codeindex

// 批量（全量）索引：并发度 + 批量大小，且不阻塞检索。
//
// 设计约束（P2 索引线全量验收）：
//  1. 并发度与批量大小可配：Concurrency 个 worker 并行解析/写库；BatchSize 决定进度回报与
//     checkpoint 粒度（每 BatchSize 个文件回报一次）；InsertBatch 决定单条 INSERT 的行数，
//     EmbedBatch 决定单次 embedding 调用的文本条数——两者共同压低往返次数。
//  2. 全量期间读旧索引不阻塞：全量复用的仍是「单文件单事务」——同一事务里先 DELETE 该文件的
//     chunks/edges、再 INSERT 新版本、最后 upsert indexed_files。PostgreSQL 的 MVCC 让并发检索
//     在提交前继续读到旧版本、提交后原子切到新版本。因此全量过程没有 TRUNCATE、没有跨文件的
//     批量 DELETE，也不持有任何表级锁。
//  3. 首次全量可查：单个文件一旦提交就进入检索可见集合，不需要等整批跑完。
//  4. 并发上限按连接池留出余量（默认 maxConns-1），保证全量期间检索仍然拿得到连接。

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultBatchConcurrency 是默认并发度；批量写库是 I/O 密集，4 路在 8 连接池下已接近饱和。
	DefaultBatchConcurrency = 4
	// MaxBatchConcurrency 是并发度硬上限，防止把连接池和 PG 的 autovacuum 一起压垮。
	MaxBatchConcurrency = 16
	// DefaultBatchSize 是每批文件数（进度回报与 checkpoint 粒度）。
	DefaultBatchSize = 32
	// DefaultInsertBatch 是单条多行 INSERT 的行数上限；14 个绑定参数/行，200 行远低于 65535 上限。
	DefaultInsertBatch = 200
)

var ErrBatchRootRequired = errors.New("codeindex: batch root is required")

// BatchFile 是一个待索引文件。Path 为绝对路径，Rel 为相对 root 的斜杠路径（用于稳定排序）。
type BatchFile struct {
	Path  string
	Rel   string
	Size  int64
	MTime time.Time
}

// BatchOptions 描述一次批量索引。
type BatchOptions struct {
	TenantID  string
	ProjectID string
	// Root 是待索引根目录；为空直接报错。
	Root string
	// Concurrency 是并发 worker 数；<=0 取 DefaultBatchConcurrency，且始终被
	// min(MaxBatchConcurrency, 连接池上限-1) 夹住。
	Concurrency int
	// BatchSize 是每批文件数；<=0 取 DefaultBatchSize。
	BatchSize int
	// InsertBatch 是单条多行 INSERT 的行数；<=0 取 DefaultInsertBatch。
	InsertBatch int
	// EmbedBatch 是单次 embedding 的文本条数；<=0 沿用 writer 默认。
	EmbedBatch int
	// Force 为 true 时忽略 content_hash 跳过逻辑（全量语义）。
	Force bool
	// Extensions 非空时只索引这些后缀（如 []string{".go"}）。
	Extensions []string
	// MaxFiles > 0 时只取排序后的前 N 个文件。
	MaxFiles int
	// Ignore 为零值时用 DefaultIgnore()。
	Ignore Ignore
	// Progress 在每个批次边界与结束时回调一次；可为 nil。
	Progress func(BatchProgress)
}

// BatchProgress 是一次批次边界的快照，用于 CLI 进度与 checkpoint 观测。
type BatchProgress struct {
	Total     int
	Done      int
	Batch     int
	Batches   int
	Indexed   int
	Skipped   int
	Failed    int
	Chunks    int
	Edges     int
	Elapsed   time.Duration
	LastPath  string
	LastError string
}

// BatchFailure 记录一个失败文件，便于 CLI 汇总后统一 exit code。
type BatchFailure struct {
	Path string
	Err  error
}

// BatchStats 是一次批量索引的汇总。P50/P95/P99 是「单文件耗时」的分位数。
type BatchStats struct {
	Total       int
	Indexed     int
	Skipped     int
	Failed      int
	Batches     int
	Chunks      int
	Edges       int
	Bytes       int64
	Concurrency int
	BatchSize   int
	Force       bool
	Elapsed     time.Duration
	P50         time.Duration
	P95         time.Duration
	P99         time.Duration
	Max         time.Duration
	Failures    []BatchFailure
}

// Percentile 返回单文件耗时分位数；data 为空时返回 0。
func (s BatchStats) Percentile(data []time.Duration, p float64) time.Duration {
	return Percentile(data, p)
}

// Summary 是 CLI 非 JSON 模式的一行摘要。
func (s BatchStats) Summary() string {
	return fmt.Sprintf("files=%d indexed=%d skipped=%d failed=%d chunks=%d edges=%d bytes=%d "+
		"concurrency=%d batch=%d elapsed=%s p50=%s p95=%s p99=%s max=%s",
		s.Total, s.Indexed, s.Skipped, s.Failed, s.Chunks, s.Edges, s.Bytes,
		s.Concurrency, s.BatchSize, s.Elapsed.Round(time.Millisecond),
		s.P50.Round(time.Millisecond), s.P95.Round(time.Millisecond),
		s.P99.Round(time.Millisecond), s.Max.Round(time.Millisecond))
}

// PoolStatus 暴露连接池占用，供 CLI 与压测脚本判断全量期间还剩多少余量给检索。
type PoolStatus struct {
	MaxConns      int32
	TotalConns    int32
	AcquiredConns int32
	IdleConns     int32
}

// PoolStatus 读取 pgxpool 的即时统计；writer 未装配 DB 时返回零值。
func (w *CodeWriter) PoolStatus() PoolStatus {
	if w == nil || w.db == nil || w.db.Pool() == nil {
		return PoolStatus{}
	}
	stat := w.db.Pool().Stat()
	return PoolStatus{
		MaxConns:      stat.MaxConns(),
		TotalConns:    stat.TotalConns(),
		AcquiredConns: stat.AcquiredConns(),
		IdleConns:     stat.IdleConns(),
	}
}

// maxConns 返回连接池上限，用于计算并发度余量。
func (w *CodeWriter) maxConns() int {
	if w == nil || w.db == nil || w.db.Pool() == nil {
		return 0
	}
	return int(w.db.Pool().Stat().MaxConns())
}

// normalized 补默认值并把并发度夹进安全区间：至少留 1 条连接给检索。
func (o BatchOptions) normalized(maxConns int) BatchOptions {
	if o.Concurrency <= 0 {
		o.Concurrency = DefaultBatchConcurrency
	}
	if o.BatchSize <= 0 {
		o.BatchSize = DefaultBatchSize
	}
	if o.InsertBatch <= 0 {
		o.InsertBatch = DefaultInsertBatch
	}
	if o.InsertBatch > 1000 {
		o.InsertBatch = 1000
	}
	limit := MaxBatchConcurrency
	if maxConns > 1 && maxConns-1 < limit {
		limit = maxConns - 1
	}
	if limit < 1 {
		limit = 1
	}
	if o.Concurrency > limit {
		o.Concurrency = limit
	}
	if o.MaxFiles < 0 {
		o.MaxFiles = 0
	}
	if o.Ignore.DirNames == nil {
		o.Ignore = DefaultIgnore()
	}
	return o
}

// IndexBatch 按 Concurrency/BatchSize 并发索引 Root 下的文件。
//
// 返回的 error 仅覆盖「整体不可继续」的错误（无 DB、缺租户、Root 不存在、ctx 取消）；
// 单文件失败记进 stats.Failures，不中断整批，由调用方决定 exit code。
func (w *CodeWriter) IndexBatch(ctx context.Context, opts BatchOptions) (BatchStats, error) {
	if w == nil || w.db == nil {
		return BatchStats{}, errors.New("codeindex: batch indexer has no database")
	}
	if w.parse == nil {
		return BatchStats{}, ErrNoParser
	}
	if strings.TrimSpace(opts.TenantID) == "" || strings.TrimSpace(opts.ProjectID) == "" {
		return BatchStats{}, errors.New("codeindex: tenant_id and project_id are required")
	}
	if strings.TrimSpace(opts.Root) == "" {
		return BatchStats{}, ErrBatchRootRequired
	}

	norm := opts.normalized(w.maxConns())
	files, err := CollectBatchFiles(norm.Root, norm.Ignore, norm.Extensions)
	if err != nil {
		return BatchStats{}, err
	}
	if norm.MaxFiles > 0 && len(files) > norm.MaxFiles {
		files = files[:norm.MaxFiles]
	}

	started := time.Now()
	stats := BatchStats{
		Total:       len(files),
		Concurrency: norm.Concurrency,
		BatchSize:   norm.BatchSize,
		Force:       norm.Force,
	}
	if len(files) == 0 {
		stats.Elapsed = time.Since(started)
		emitProgress(norm.Progress, stats, 0, "")
		return stats, nil
	}

	indexOpts := IndexOptions{
		Force:           norm.Force,
		SkipStateLookup: norm.Force,
		InsertBatch:     norm.InsertBatch,
		EmbedBatch:      norm.EmbedBatch,
	}

	type outcome struct {
		file   BatchFile
		result IndexResult
		err    error
	}

	jobs := make(chan BatchFile)
	results := make(chan outcome)
	var wg sync.WaitGroup
	for i := 0; i < norm.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for file := range jobs {
				src, readErr := os.ReadFile(file.Path)
				if readErr != nil {
					results <- outcome{file: file, err: fmt.Errorf("read: %w", readErr)}
					continue
				}
				res, indexErr := w.IndexFileWith(ctx, norm.TenantID, norm.ProjectID, file.Path, src, file.MTime, indexOpts)
				results <- outcome{file: file, result: res, err: indexErr}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, file := range files {
			select {
			case <-ctx.Done():
				return
			case jobs <- file:
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	var (
		durations = make([]time.Duration, 0, len(files))
		done      int
		lastPath  string
	)
	for out := range results {
		done++
		stats.Bytes += out.file.Size
		lastPath = out.file.Rel
		if out.err != nil {
			stats.Failed++
			stats.Failures = append(stats.Failures, BatchFailure{Path: out.file.Path, Err: out.err})
		} else if out.result.Skipped {
			stats.Skipped++
		} else {
			stats.Indexed++
			stats.Chunks += out.result.Chunks
			stats.Edges += out.result.Edges
			durations = append(durations, out.result.Duration)
		}
		if done%norm.BatchSize == 0 || done == len(files) {
			stats.Batches++
			partial := stats
			partial.Elapsed = time.Since(started)
			emitProgress(norm.Progress, partial, done, lastPath)
		}
	}

	stats.Elapsed = time.Since(started)
	stats.P50 = Percentile(durations, 0.50)
	stats.P95 = Percentile(durations, 0.95)
	stats.P99 = Percentile(durations, 0.99)
	stats.Max = Percentile(durations, 1.0)
	if done < len(files) && ctx.Err() != nil {
		return stats, ctx.Err()
	}
	return stats, nil
}

// IndexFull 是全量语义的入口：强制重写全部文件，并跳过 content_hash 预检的额外往返。
// 因为每个文件仍在自己的事务里提交，全量期间检索继续读到旧索引（见文件头注释）。
func (w *CodeWriter) IndexFull(ctx context.Context, opts BatchOptions) (BatchStats, error) {
	opts.Force = true
	return w.IndexBatch(ctx, opts)
}

func emitProgress(hook func(BatchProgress), stats BatchStats, done int, lastPath string) {
	if hook == nil {
		return
	}
	progress := BatchProgress{
		Total:    stats.Total,
		Done:     done,
		Batch:    stats.BatchSize,
		Batches:  stats.Batches,
		Indexed:  stats.Indexed,
		Skipped:  stats.Skipped,
		Failed:   stats.Failed,
		Chunks:   stats.Chunks,
		Edges:    stats.Edges,
		Elapsed:  stats.Elapsed,
		LastPath: lastPath,
	}
	if n := len(stats.Failures); n > 0 {
		progress.LastError = stats.Failures[n-1].Err.Error()
	}
	hook(progress)
}

// CollectBatchFiles 以确定的顺序（相对路径字典序）收集 root 下的待索引文件。
//
// 跳过规则：Ignore.DirNames 命中的目录整棵跳过；Ignore.ShouldIgnore 命中的文件跳过；
// Ignore.TooLarge 命中跳过；符号链接跳过（避免走出 root）。
func CollectBatchFiles(root string, ig Ignore, extensions []string) ([]BatchFile, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("codeindex: resolve root: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("codeindex: stat root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("codeindex: root is not a directory: %s", abs)
	}
	if ig.DirNames == nil {
		ig = DefaultIgnore()
	}
	exts := normalizeExtensions(extensions)

	var files []BatchFile
	walkErr := filepath.WalkDir(abs, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			if path != abs && ig.DirNames[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return nil
		}
		if ig.ShouldIgnore(path) {
			return nil
		}
		if len(exts) > 0 && !exts[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		stat, statErr := entry.Info()
		if statErr != nil {
			return nil
		}
		if ig.TooLarge(stat.Size()) {
			return nil
		}
		rel, relErr := filepath.Rel(abs, path)
		if relErr != nil {
			return nil
		}
		files = append(files, BatchFile{
			Path:  path,
			Rel:   filepath.ToSlash(rel),
			Size:  stat.Size(),
			MTime: stat.ModTime(),
		})
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("codeindex: walk root: %w", walkErr)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Rel < files[j].Rel })
	return files, nil
}

func normalizeExtensions(extensions []string) map[string]bool {
	if len(extensions) == 0 {
		return nil
	}
	out := make(map[string]bool, len(extensions))
	for _, ext := range extensions {
		ext = strings.ToLower(strings.TrimSpace(ext))
		if ext == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		out[ext] = true
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Percentile 返回分位数（最近秩法），空集合返回 0。
func Percentile(data []time.Duration, p float64) time.Duration {
	if len(data) == 0 {
		return 0
	}
	if p >= 1 {
		max := data[0]
		for _, d := range data[1:] {
			if d > max {
				max = d
			}
		}
		return max
	}
	sorted := append([]time.Duration(nil), data...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted))*p+0.999999) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
