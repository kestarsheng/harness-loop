package main

// bench/repo-500 是做 P2 索引线全量压测的固定夹具 + 压测脚本。
//
// 固定两件事，否则数字不可比：
//  1. 仓库快照：文件内容完全由 (index, seed) 决定，snapshot.json 记录每个文件的 sha256
//     与整体 digest；压测前先校验，任一处不符直接拒绝出数。
//  2. 硬件与运行时：hardware.json 记录基准机器的 CPU/核数/内存/OS/Go/PG 版本与 GOMAXPROCS，
//     压测时按记录值固定 GOMAXPROCS，并把实测硬件写进结果（不匹配时标注，不静默）。
//
// 用法：
//
//	go run ./bench/repo-500/cmd -mode gen   -files 500
//	go run ./bench/repo-500/cmd -mode check
//	go run ./bench/repo-500/cmd -mode run   -dsn "$HARNESS_TEST_PG_DSN"

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"agent-platform/pkg/codeindex"
	"agent-platform/pkg/codeindex/parser"
	"agent-platform/pkg/pgindex"
)

const (
	generatorVersion = 1
	snapshotSeed     = "hlp-p2-index-batch/repo-500/v1"

	manifestName = "snapshot.json"
	hardwareName = "hardware.json"
)

// manifest 是仓库快照的指纹文件，随仓库提交，压测前逐条校验。
type manifest struct {
	Name             string         `json:"name"`
	Files            int            `json:"files"`
	TotalBytes       int64          `json:"total_bytes"`
	Generator        string         `json:"generator"`
	GeneratorVersion int            `json:"generator_version"`
	Seed             string         `json:"seed"`
	Digest           string         `json:"digest"`
	Entries          []manifestFile `json:"entries"`
}

type manifestFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

// hardwareProfile 固定基准硬件与运行时参数。
type hardwareProfile struct {
	CPUModel    string `json:"cpu_model"`
	Cores       int    `json:"cores"`
	RAMGB       int    `json:"ram_gb"`
	OS          string `json:"os"`
	GoVersion   string `json:"go_version"`
	PGVersion   string `json:"pg_version"`
	GOMAXPROCS  int    `json:"gomaxprocs"`
	Concurrency int    `json:"concurrency"`
	InsertBatch int    `json:"insert_batch"`
	BatchSize   int    `json:"batch_size"`
	Note        string `json:"note"`
}

func main() {
	mode := flag.String("mode", "run", "gen | check | run")
	dir := flag.String("dir", filepath.Join("bench", "repo-500", "testdata", "repo"), "fixture directory (keep it under testdata so the go tool ignores it)")
	count := flag.Int("files", 0, "file count (0 = value in snapshot.json, or 500 when generating)")
	dsn := flag.String("dsn", os.Getenv("HARNESS_TEST_PG_DSN"), "PostgreSQL DSN")
	tenant := flag.String("tenant", "tenant-bench-repo500", "tenant id")
	project := flag.String("project", "proj-bench-repo500", "project id")
	concurrency := flag.Int("concurrency", 0, "indexing concurrency (0 = hardware.json value)")
	batchSize := flag.Int("batch", 0, "files per batch (0 = hardware.json value)")
	insertBatch := flag.Int("insert-batch", 0, "rows per multi-row INSERT (0 = hardware.json value)")
	probeEvery := flag.Duration("probe-interval", 500*time.Millisecond, "search probe interval during full index")
	probeQuery := flag.String("probe-query", "handler", "query used by the search probe")
	sample := flag.Int("sample", 20, "files used for the sequential single-file latency sample")
	jsonOut := flag.Bool("json", false, "emit JSON only")
	out := flag.String("out", "", "write the JSON result to this path")
	flag.Parse()

	root := *dir
	if err := os.MkdirAll(root, 0o755); err != nil {
		fatal("mkdir %s: %v", root, err)
	}
	profile := loadHardware(root)

	switch *mode {
	case "gen":
		n := *count
		if n <= 0 {
			n = 500
		}
		man, err := generate(root, n)
		if err != nil {
			fatal("generate: %v", err)
		}
		if err := writeJSON(filepath.Join(root, manifestName), man); err != nil {
			fatal("write manifest: %v", err)
		}
		fmt.Printf("generated %d files (%d bytes) digest=%s\n", man.Files, man.TotalBytes, man.Digest)
	case "check":
		man, err := verify(root)
		if err != nil {
			fatal("snapshot check failed: %v", err)
		}
		fmt.Printf("snapshot OK: %d files, %d bytes, digest=%s\n", man.Files, man.TotalBytes, man.Digest)
	case "run":
		result, err := run(root, profile, runConfig{
			DSN:         *dsn,
			TenantID:    *tenant,
			ProjectID:   *project,
			Concurrency: *concurrency,
			BatchSize:   *batchSize,
			InsertBatch: *insertBatch,
			ProbeEvery:  *probeEvery,
			ProbeQuery:  *probeQuery,
			Sample:      *sample,
			Files:       *count,
		})
		if err != nil {
			fatal("bench: %v", err)
		}
		if *out != "" {
			if err := writeJSON(*out, result); err != nil {
				fatal("write result: %v", err)
			}
		}
		if *jsonOut {
			raw, _ := json.MarshalIndent(result, "", "  ")
			fmt.Println(string(raw))
		} else {
			fmt.Print(result.Render())
		}
		if !result.Passed {
			os.Exit(1)
		}
	default:
		fatal("unknown mode %q", *mode)
	}
}

// ---------- 夹具生成 ----------

// generate 在 root 下写出 count 个文件并返回指纹。
func generate(root string, count int) (manifest, error) {
	man := manifest{
		Name:             fmt.Sprintf("repo-%d", count),
		Files:            count,
		Generator:        "bench/repo-500/cmd",
		GeneratorVersion: generatorVersion,
		Seed:             snapshotSeed,
		Entries:          make([]manifestFile, 0, count),
	}
	files := buildFiles(count)
	for _, file := range files {
		full := filepath.Join(root, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return man, err
		}
		if err := os.WriteFile(full, []byte(file.Content), 0o644); err != nil {
			return man, err
		}
		man.Entries = append(man.Entries, manifestFile{
			Path:   file.Path,
			SHA256: sha256Hex([]byte(file.Content)),
			Bytes:  len(file.Content),
		})
		man.TotalBytes += int64(len(file.Content))
	}
	man.Digest = digestOf(man.Entries)
	return man, nil
}

// verify 逐条校验磁盘内容与 snapshot.json 一致，返回清单。
func verify(root string) (manifest, error) {
	var man manifest
	raw, err := os.ReadFile(filepath.Join(root, manifestName))
	if err != nil {
		return man, fmt.Errorf("read %s: %w (run -mode gen first)", manifestName, err)
	}
	if err := json.Unmarshal(raw, &man); err != nil {
		return man, fmt.Errorf("parse %s: %w", manifestName, err)
	}
	if man.GeneratorVersion != generatorVersion {
		return man, fmt.Errorf("generator version drift: snapshot=%d current=%d", man.GeneratorVersion, generatorVersion)
	}
	var mismatches []string
	for _, entry := range man.Entries {
		full := filepath.Join(root, filepath.FromSlash(entry.Path))
		content, err := os.ReadFile(full)
		if err != nil {
			mismatches = append(mismatches, entry.Path+": missing")
			continue
		}
		if got := sha256Hex(content); got != entry.SHA256 {
			mismatches = append(mismatches, entry.Path+": sha256 "+got+" != "+entry.SHA256)
		}
	}
	if len(mismatches) > 0 {
		sort.Strings(mismatches)
		head := mismatches
		if len(head) > 5 {
			head = head[:5]
		}
		return man, fmt.Errorf("%d file(s) drifted from snapshot; first: %s", len(mismatches), strings.Join(head, "; "))
	}
	if got := digestOf(man.Entries); got != man.Digest {
		return man, fmt.Errorf("manifest digest self-check failed: %s != %s", got, man.Digest)
	}
	return man, nil
}

type generated struct {
	Path    string
	Content string
}

// buildFiles 是确定性的文件内容生成器：只依赖 index 与固定 seed，跨机器字节级一致。
func buildFiles(count int) []generated {
	files := make([]generated, 0, count)
	for i := 0; i < count; i++ {
		pkg := fmt.Sprintf("pkg/svc%03d", i/5)
		path := fmt.Sprintf("%s/svc%03d.go", pkg, i)
		files = append(files, generated{Path: path, Content: buildSource(i, pkg)})
	}
	return files
}

func buildSource(i int, pkg string) string {
	funcs := 6 + i%7
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\n", strings.ReplaceAll(pkg, "/", ""))
	b.WriteString("import (\n\t\"fmt\"\n\t\"strings\"\n)\n\n")
	fmt.Fprintf(&b, "// Service%03d is one service unit of the fixture repo; handler code lives below.\n", i)
	fmt.Fprintf(&b, "type Service%03d struct {\n\tID   int\n\tName string\n}\n\n", i)
	fmt.Fprintf(&b, "func NewService%03d(id int) *Service%03d {\n\treturn &Service%03d{ID: id, Name: fmt.Sprintf(\"svc-%%d\", id)}\n}\n\n", i, i, i)
	for f := 0; f < funcs; f++ {
		name := fmt.Sprintf("Handler%03d%02d", i, f)
		fmt.Fprintf(&b, "// %s is the handler for request class %d; the call chain is fixed so edges stay stable.\n", name, f)
		fmt.Fprintf(&b, "func %s(input string) string {\n", name)
		fmt.Fprintf(&b, "\ttrimmed := strings.TrimSpace(input)\n")
		fmt.Fprintf(&b, "\tif trimmed == \"\" {\n\t\ttrimmed = \"svc-%d-func-%d\"\n\t}\n", i, f)
		if f+1 < funcs {
			fmt.Fprintf(&b, "\treturn %s%02d(strings.ToUpper(trimmed))\n", fmt.Sprintf("Handler%03d", i), f+1)
		} else {
			fmt.Fprintf(&b, "\treturn fmt.Sprintf(\"%%s/%%d\", strings.ToUpper(trimmed), %d)\n", i)
		}
		b.WriteString("}\n\n")
	}
	fmt.Fprintf(&b, "func (s *Service%03d) Run(input string) string {\n\treturn Handler%03d00(input)\n}\n", i, i)
	return b.String()
}

func sha256Hex(src []byte) string {
	sum := sha256.Sum256(src)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func digestOf(entries []manifestFile) string {
	h := sha256.New()
	sorted := append([]manifestFile(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	for _, entry := range sorted {
		fmt.Fprintf(h, "%s:%s\n", entry.Path, entry.SHA256)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// ---------- 硬件画像 ----------

func loadHardware(root string) hardwareProfile {
	var profile hardwareProfile
	raw, err := os.ReadFile(filepath.Join(root, hardwareName))
	if err == nil {
		if err := json.Unmarshal(raw, &profile); err != nil {
			profile = hardwareProfile{}
		}
	}
	if profile.Concurrency <= 0 {
		profile.Concurrency = codeindex.DefaultBatchConcurrency
	}
	if profile.BatchSize <= 0 {
		profile.BatchSize = codeindex.DefaultBatchSize
	}
	if profile.InsertBatch <= 0 {
		profile.InsertBatch = codeindex.DefaultInsertBatch
	}
	if profile.GOMAXPROCS > 0 {
		runtime.GOMAXPROCS(profile.GOMAXPROCS)
	}
	return profile
}

func currentHardware(profile hardwareProfile) hardwareProfile {
	now := profile
	now.CPUModel = detectCPUModel()
	now.OS = runtime.GOOS + "/" + runtime.GOARCH
	now.Cores = runtime.NumCPU()
	now.GoVersion = runtime.Version()
	now.GOMAXPROCS = runtime.GOMAXPROCS(0)
	return now
}

// hardwareMatches 判断当前机器是否就是 hardware.json 记录的基准机。
// 硬闸门是「逻辑核数 + 操作系统 + GOMAXPROCS」——这三项决定吞吐量级；
// CPU 型号字符串在不同系统上取到的形式不同（Windows 只给 Family/Model），
// 只作提示写进结果，不参与判定，避免基准机自家人打自家人。
func hardwareMatches(ref, actual hardwareProfile) bool {
	if actual.Cores == 0 || actual.Cores != ref.Cores {
		return false
	}
	if ref.OS != "" && actual.OS != "" && !strings.EqualFold(ref.OS, actual.OS) {
		return false
	}
	if ref.GOMAXPROCS > 0 && actual.GOMAXPROCS != ref.GOMAXPROCS {
		return false
	}
	return true
}

// detectCPUModel 尽量取到 CPU 型号；取不到就返回空串（hardwareMatches 会退化为比核数）。
func detectCPUModel() string {
	switch runtime.GOOS {
	case "windows":
		return strings.TrimSpace(os.Getenv("PROCESSOR_IDENTIFIER"))
	case "linux":
		raw, err := os.ReadFile("/proc/cpuinfo")
		if err != nil {
			return ""
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "model name") {
				if idx := strings.Index(line, ":"); idx >= 0 {
					return strings.TrimSpace(line[idx+1:])
				}
			}
		}
	}
	return ""
}

// ---------- 压测 ----------

type runConfig struct {
	DSN         string
	TenantID    string
	ProjectID   string
	Concurrency int
	BatchSize   int
	InsertBatch int
	ProbeEvery  time.Duration
	ProbeQuery  string
	Sample      int
	Files       int
}

type phaseStats struct {
	Name        string  `json:"name"`
	Files       int     `json:"files"`
	Indexed     int     `json:"indexed"`
	Skipped     int     `json:"skipped"`
	Failed      int     `json:"failed"`
	Chunks      int     `json:"chunks"`
	Edges       int     `json:"edges"`
	Bytes       int64   `json:"bytes"`
	ElapsedMS   int64   `json:"elapsed_ms"`
	ThroughputF float64 `json:"files_per_sec"`
	P50MS       int64   `json:"p50_ms"`
	P95MS       int64   `json:"p95_ms"`
	P99MS       int64   `json:"p99_ms"`
	MaxMS       int64   `json:"max_ms"`
}

type probeStats struct {
	Probes      int     `json:"probes"`
	OK          int     `json:"ok"`
	Failed      int     `json:"failed"`
	SuccessRate float64 `json:"success_rate"`
	P50MS       int64   `json:"p50_ms"`
	P95MS       int64   `json:"p95_ms"`
	MaxMS       int64   `json:"max_ms"`
	LastError   string  `json:"last_error,omitempty"`
}

// snapshotRef 是结果里引用的快照指纹（不带 500 条 entries，避免结果文件膨胀）。
type snapshotRef struct {
	Name             string `json:"name"`
	Files            int    `json:"files"`
	TotalBytes       int64  `json:"total_bytes"`
	Digest           string `json:"digest"`
	GeneratorVersion int    `json:"generator_version"`
}

type benchResult struct {
	Snapshot     snapshotRef     `json:"snapshot"`
	Hardware     hardwareProfile `json:"hardware"`
	HardwareRef  hardwareProfile `json:"hardware_reference"`
	HardwareHit  bool            `json:"hardware_match"`
	DSNHost      string          `json:"dsn_host"`
	Full         phaseStats      `json:"full_index"`
	SingleSample phaseStats      `json:"single_file_sample"`
	Batch100     phaseStats      `json:"batch_100"`
	Probe        probeStats      `json:"search_probe"`
	VerifyHits   int             `json:"verify_hits"`
	Budgets      map[string]bool `json:"budgets"`
	Passed       bool            `json:"passed"`
	Notes        []string        `json:"notes"`
}

func run(root string, profile hardwareProfile, cfg runConfig) (benchResult, error) {
	result := benchResult{
		HardwareRef: profile,
		Hardware:    currentHardware(profile),
		Budgets:     map[string]bool{},
	}
	man, err := verify(root)
	if err != nil {
		return result, fmt.Errorf("snapshot: %w", err)
	}
	result.Snapshot = snapshotRef{
		Name:             man.Name,
		Files:            man.Files,
		TotalBytes:       man.TotalBytes,
		Digest:           man.Digest,
		GeneratorVersion: man.GeneratorVersion,
	}
	result.HardwareHit = hardwareMatches(profile, result.Hardware)

	if cfg.DSN == "" {
		return result, errors.New("no DSN: pass -dsn or set HARNESS_TEST_PG_DSN")
	}
	result.DSNHost = maskDSN(cfg.DSN)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	pgCfg := pgindex.DefaultConfig(cfg.DSN)
	if cfg.Concurrency+2 > int(pgCfg.MaxConns) {
		pgCfg.MaxConns = int32(cfg.Concurrency + 2)
	}
	db, err := pgindex.Open(ctx, pgCfg)
	if err != nil {
		return result, fmt.Errorf("open pg: %w", err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		return result, fmt.Errorf("migrate: %w", err)
	}
	var pgVersion string
	if err := db.QueryRow(ctx, "SHOW server_version").Scan(&pgVersion); err == nil {
		result.Hardware.PGVersion = "postgres " + pgVersion
	}

	registry := parser.NewRegistry()
	writer := codeindex.NewCodeWriter(db, func(path string, src []byte) (codeindex.ParseResult, error) {
		return registry.Parse(path, src), nil
	}, nil, nil)

	if err := purge(ctx, db, cfg.TenantID, cfg.ProjectID); err != nil {
		return result, err
	}

	base := codeindex.BatchOptions{
		TenantID:    cfg.TenantID,
		ProjectID:   cfg.ProjectID,
		Root:        root,
		Concurrency: cfg.Concurrency,
		BatchSize:   cfg.BatchSize,
		InsertBatch: cfg.InsertBatch,
		Force:       true,
		Extensions:  []string{".go"},
		MaxFiles:    cfg.Files,
	}

	// 1) 首次全量：边索引边检索，验证「读旧索引不阻塞」。
	stopProbe := make(chan struct{})
	probeCh := make(chan probeStats, 1)
	go func() {
		probeCh <- probeDuringIndex(ctx, writer, cfg.TenantID, cfg.ProjectID, cfg.ProbeQuery, cfg.ProbeEvery, stopProbe)
	}()
	fullStart := time.Now()
	full, err := writer.IndexFull(ctx, base)
	close(stopProbe)
	result.Probe = <-probeCh
	if err != nil {
		return result, fmt.Errorf("full index: %w", err)
	}
	result.Full = toPhase("full_index", full, time.Since(fullStart))

	// 全量后必须可查（首次全量可查）。
	if hits, _, searchErr := writer.Search(ctx, codeindex.SearchOptions{
		TenantID: cfg.TenantID, ProjectID: cfg.ProjectID, Query: cfg.ProbeQuery, TopK: 5,
	}); searchErr == nil {
		result.VerifyHits = len(hits)
	} else {
		result.Notes = append(result.Notes, "post-full search failed: "+searchErr.Error())
	}

	// 2) 单文件延迟：顺序重写 sample 个文件，取 P95/P99。
	sampleFiles, err := codeindex.CollectBatchFiles(root, codeindex.DefaultIgnore(), []string{".go"})
	if err != nil {
		return result, err
	}
	n := cfg.Sample
	if n <= 0 {
		n = 20
	}
	if n > len(sampleFiles) {
		n = len(sampleFiles)
	}
	single, err := measureSingle(ctx, writer, cfg, sampleFiles[:n])
	if err != nil {
		return result, err
	}
	result.SingleSample = single

	// 3) 100 文件批量：整批 100 个文件的 per-file P95。
	batchN := 100
	if batchN > len(sampleFiles) {
		batchN = len(sampleFiles)
	}
	batchOpts := base
	batchOpts.MaxFiles = batchN
	batchStart := time.Now()
	batch, err := writer.IndexFull(ctx, batchOpts)
	if err != nil {
		return result, fmt.Errorf("100-file batch: %w", err)
	}
	result.Batch100 = toPhase("batch_100", batch, time.Since(batchStart))

	// 预算判定：单文件 P95≤2s / P99≤5s；100 文件批量 P95≤60s；批量期间检索成功率 100%。
	result.Budgets["single_p95_le_2s"] = result.SingleSample.P95MS <= 2000
	result.Budgets["single_p99_le_5s"] = result.SingleSample.P99MS <= 5000
	result.Budgets["batch100_p95_le_60s"] = result.Batch100.P95MS <= 60000
	result.Budgets["probe_success_100"] = result.Probe.Probes > 0 && result.Probe.Failed == 0
	result.Budgets["full_index_searchable"] = result.VerifyHits > 0
	result.Passed = true
	for _, ok := range result.Budgets {
		if !ok {
			result.Passed = false
		}
	}
	if !result.HardwareHit {
		result.Notes = append(result.Notes, "hardware differs from hardware.json reference (cores/os/gomaxprocs); numbers are not directly comparable")
	}
	if ref := result.HardwareRef.CPUModel; ref != "" && !strings.EqualFold(ref, result.Hardware.CPUModel) {
		result.Notes = append(result.Notes, fmt.Sprintf("cpu string differs from reference (%q vs %q) — informational only", result.Hardware.CPUModel, ref))
	}
	return result, nil
}

func toPhase(name string, stats codeindex.BatchStats, elapsed time.Duration) phaseStats {
	phase := phaseStats{
		Name:      name,
		Files:     stats.Total,
		Indexed:   stats.Indexed,
		Skipped:   stats.Skipped,
		Failed:    stats.Failed,
		Chunks:    stats.Chunks,
		Edges:     stats.Edges,
		Bytes:     stats.Bytes,
		ElapsedMS: elapsed.Milliseconds(),
		P50MS:     stats.P50.Milliseconds(),
		P95MS:     stats.P95.Milliseconds(),
		P99MS:     stats.P99.Milliseconds(),
		MaxMS:     stats.Max.Milliseconds(),
	}
	if elapsed > 0 {
		phase.ThroughputF = float64(stats.Indexed) / elapsed.Seconds()
	}
	return phase
}

// measureSingle 顺序重写 n 个文件，得到单文件延迟分布（不受并发互相干扰）。
func measureSingle(ctx context.Context, writer *codeindex.CodeWriter, cfg runConfig, files []codeindex.BatchFile) (phaseStats, error) {
	durations := make([]time.Duration, 0, len(files))
	stats := phaseStats{Name: "single_file_sample"}
	start := time.Now()
	for _, file := range files {
		src, err := os.ReadFile(file.Path)
		if err != nil {
			return stats, err
		}
		res, err := writer.IndexFileWith(ctx, cfg.TenantID, cfg.ProjectID, file.Path, src, file.MTime, codeindex.IndexOptions{
			Force:           true,
			SkipStateLookup: true,
			InsertBatch:     cfg.InsertBatch,
		})
		if err != nil {
			return stats, fmt.Errorf("index %s: %w", file.Rel, err)
		}
		durations = append(durations, res.Duration)
		stats.Chunks += res.Chunks
		stats.Edges += res.Edges
	}
	elapsed := time.Since(start)
	stats.Files = len(files)
	stats.Indexed = len(files)
	stats.ElapsedMS = elapsed.Milliseconds()
	stats.P50MS = codeindex.Percentile(durations, 0.50).Milliseconds()
	stats.P95MS = codeindex.Percentile(durations, 0.95).Milliseconds()
	stats.P99MS = codeindex.Percentile(durations, 0.99).Milliseconds()
	stats.MaxMS = codeindex.Percentile(durations, 1.0).Milliseconds()
	if elapsed > 0 {
		stats.ThroughputF = float64(len(files)) / elapsed.Seconds()
	}
	return stats, nil
}

// probeDuringIndex 全量期间反复检索，统计成功率与延迟。
func probeDuringIndex(ctx context.Context, writer *codeindex.CodeWriter, tenantID, projectID, query string,
	every time.Duration, stop <-chan struct{}) probeStats {
	if every <= 0 {
		every = 500 * time.Millisecond
	}
	stats := probeStats{}
	var latencies []time.Duration
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			finishProbe(&stats, latencies)
			return stats
		case <-ctx.Done():
			finishProbe(&stats, latencies)
			return stats
		case <-ticker.C:
			probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, searchStats, err := writer.Search(probeCtx, codeindex.SearchOptions{
				TenantID: tenantID, ProjectID: projectID, Query: query, TopK: 5,
			})
			cancel()
			stats.Probes++
			if err != nil {
				stats.Failed++
				stats.LastError = err.Error()
				continue
			}
			stats.OK++
			latencies = append(latencies, time.Duration(searchStats.LatencyMs)*time.Millisecond)
		}
	}
}

func finishProbe(stats *probeStats, latencies []time.Duration) {
	if stats.Probes > 0 {
		stats.SuccessRate = float64(stats.OK) / float64(stats.Probes) * 100
	}
	if len(latencies) == 0 {
		return
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	stats.P50MS = latencies[len(latencies)/2].Milliseconds()
	stats.P95MS = codeindex.Percentile(latencies, 0.95).Milliseconds()
	stats.MaxMS = latencies[len(latencies)-1].Milliseconds()
}

func purge(ctx context.Context, db *pgindex.DB, tenantID, projectID string) error {
	for _, table := range []string{"code_chunks", "code_edges", "indexed_files"} {
		if _, err := db.Exec(ctx, "DELETE FROM hlp_code."+table+" WHERE tenant_id=$1 AND project_id=$2", tenantID, projectID); err != nil {
			return fmt.Errorf("purge %s: %w", table, err)
		}
	}
	return nil
}

func (r benchResult) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "snapshot   %s files=%d bytes=%d digest=%s\n", r.Snapshot.Name, r.Snapshot.Files, r.Snapshot.TotalBytes, r.Snapshot.Digest)
	fmt.Fprintf(&b, "hardware   cpu=%q cores=%d gomaxprocs=%d %s go=%s pg=%s\n",
		r.Hardware.CPUModel, r.Hardware.Cores, r.Hardware.GOMAXPROCS, r.Hardware.OS, r.Hardware.GoVersion, r.Hardware.PGVersion)
	fmt.Fprintf(&b, "dsn        %s\n\n", r.DSNHost)
	for _, phase := range []phaseStats{r.Full, r.SingleSample, r.Batch100} {
		fmt.Fprintf(&b, "%-18s files=%-5d elapsed=%-8dms thr=%.1f files/s p50=%dms p95=%dms p99=%dms max=%dms chunks=%d edges=%d\n",
			phase.Name, phase.Files, phase.ElapsedMS, phase.ThroughputF, phase.P50MS, phase.P95MS, phase.P99MS, phase.MaxMS, phase.Chunks, phase.Edges)
	}
	fmt.Fprintf(&b, "\n%-22s probes=%d ok=%d failed=%d success=%.2f%% p50=%dms p95=%dms max=%dms\n",
		"search_probe", r.Probe.Probes, r.Probe.OK, r.Probe.Failed, r.Probe.SuccessRate, r.Probe.P50MS, r.Probe.P95MS, r.Probe.MaxMS)
	fmt.Fprintf(&b, "verify_hits=%d\n\n", r.VerifyHits)
	keys := make([]string, 0, len(r.Budgets))
	for k := range r.Budgets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "  %-26s %s\n", k, passFail(r.Budgets[k]))
	}
	fmt.Fprintf(&b, "\nPASSED=%v\n", r.Passed)
	for _, note := range r.Notes {
		fmt.Fprintf(&b, "note: %s\n", note)
	}
	return b.String()
}

func passFail(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

func maskDSN(dsn string) string {
	if i := strings.Index(dsn, "@"); i >= 0 && i+1 < len(dsn) {
		return "***@" + dsn[i+1:]
	}
	return "***"
}

func writeJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(path, raw, 0o644)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "bench/repo-500: "+format+"\n", args...)
	os.Exit(1)
}
