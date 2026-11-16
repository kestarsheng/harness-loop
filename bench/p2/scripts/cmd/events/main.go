// events 是 T-P2-T1 的「事件端到端」压测程序（DFX-P-07）。
//
// 测的是「事件从服务端产生到 SSE 客户端收到」这一段真实链路：
//
//	harnessloop-service 写 hlp_event.events
//	  → gateway WatchEvents(gRPC stream) → MonitorStream(SSE) → 本程序
//
// 延迟 = 本地收到时刻 − 事件自带的 timestamp_ms。两端同机同钟，无时钟偏差。
//
// 一行必须说清的口径：本程序同时报「延迟」和「收集率」。延迟只统计本轮任务的
// 事件（历史回放的事件 timestamp 是几十分钟前，混进来会把 P95 拉到分钟级）；
// 收集率的分母取自服务端数据库，不是「任务数 × 4」这种硬编码——FSM 路径不同，
// 每个任务产生的事件条数不固定。分子按 event_id 去重。
//
// 用法：
//
//	go run ./bench/p2/scripts/cmd/events -tasks 5 -base http://127.0.0.1:9000
//
// 前置：harnessloop-service(:50008) + gateway(:9000) + scripts/fakeagent(:50002)。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/jackc/pgx/v5"

	harnesspb "agent-platform/pkg/pb/harnessloop"
)

type sseEvent struct {
	EventID     string `json:"event_id"`
	EventType   string `json:"event_type"`
	TaskID      string `json:"task_id"`
	Stage       string `json:"stage"`
	Seq         int64  `json:"seq"`
	TimestampMs int64  `json:"timestamp_ms"`
	Severity    string `json:"severity"`
}

type arrival struct {
	EventID   string  `json:"event_id"`
	EventType string  `json:"event_type"`
	TaskID    string  `json:"task_id"`
	Seq       int64   `json:"seq"`
	LagMs     float64 `json:"lag_ms"`
}

type result struct {
	Name         string          `json:"name"`
	StartedAt    string          `json:"started_at"`
	Base         string          `json:"base"`
	StreamURL    string          `json:"stream_url"`
	Tasks        int             `json:"tasks"`
	Events       int             `json:"events"`
	Expected     int             `json:"expected_events"`
	Missing      int             `json:"missing_events"`
	Collected    float64         `json:"collection_rate"`
	SeqMonotonic bool            `json:"seq_monotonic"`
	SeqGaps      int             `json:"seq_gaps"`
	CatchupOK    bool            `json:"catchup_no_dup_no_gap"`
	CatchupSeq   []int64         `json:"catchup_seqs"`
	Dropped      int             `json:"dropped_before_tasks"`
	P50Ms        float64         `json:"p50_ms"`
	P95Ms        float64         `json:"p95_ms"`
	P99Ms        float64         `json:"p99_ms"`
	MaxMs        float64         `json:"max_ms"`
	ByType       map[string]int  `json:"by_type"`
	FirstEventMs int64           `json:"first_event_ms_after_create"`
	Budgets      map[string]bool `json:"budgets"`
	Hardware     map[string]any  `json:"hardware"`
	Passed       bool            `json:"passed"`
	Notes        []string        `json:"notes"`
	Samples      []arrival       `json:"samples"`
}

func main() {
	base := flag.String("base", "http://127.0.0.1:9000", "gateway base URL")
	addr := flag.String("addr", "127.0.0.1:50008", "harnessloop gRPC address")
	tasks := flag.Int("tasks", 5, "how many tasks to create as event sources")
	tenant := flag.String("tenant", "tenant-bench-p2t1", "tenant id")
	project := flag.String("project", "proj-bench-p2t1", "project id")
	budget := flag.Float64("budget-ms", 1000, "P95 budget in ms (DFX-P-07)")
	wait := flag.Duration("wait", 90*time.Second, "waiting budget for all tasks")
	purge := flag.Bool("purge", false, "delete this tenant's events before measuring (cold-start semantics)")
	dsn := flag.String("dsn", os.Getenv("HARNESS_TEST_PG_DSN"), "PostgreSQL DSN, required by -purge")
	out := flag.String("out", "", "write JSON result here")
	flag.Parse()

	tenantID := *tenant

	if *purge {
		if *dsn == "" {
			fatal("-purge requires -dsn or HARNESS_TEST_PG_DSN")
		}
		n, err := purgeEvents(ctx0(), *dsn, tenantID)
		if err != nil {
			fatal("purge events: %v", err)
		}
		fmt.Fprintf(os.Stderr, "events: purged %d rows for tenant %s\n", n, tenantID)
	}

	streamBase := fmt.Sprintf("%s/api/v2/harness-loop/monitor/stream?tenant=%s", *base, tenantID)
	streamURL := streamBase + "&last_seq=0"

	ctx, cancel := context.WithTimeout(context.Background(), *wait+60*time.Second)
	defer cancel()

	res := result{
		Name:         "events_e2e",
		StartedAt:    time.Now().Format(time.RFC3339),
		Base:         *base,
		StreamURL:    streamURL,
		Tasks:        *tasks,
		SeqMonotonic: true,
		ByType:       map[string]int{},
		Budgets:      map[string]bool{},
	}

	// 收集端：先连 SSE，等 catch-up 结束（收到 synced）再算「就绪」。
	//
	// 过滤只用 task_id：seq 在 T-P2-17 之后是租户级全局游标（global_seq），
	// 游标本身已经可信，但 catch-up 回放的历史事件仍然会混进来，用本轮
	// createdIDs 过一遍最省事，也不依赖时间窗。
	var (
		mu       sync.Mutex
		samples  []arrival
		syncedOK = make(chan int64, 1)
		dropped  int
		ready    = make(chan struct{})
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		fatal("build request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fatal("connect SSE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fatal("SSE status %d", resp.StatusCode)
	}

	go func() {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		var evName string
		var dataLine string
		announced := false
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				evName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				dataLine = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			case line == "":
				if evName == "synced" && !announced {
					announced = true
					var meta struct {
						LastSeq int64 `json:"last_seq"`
					}
					_ = json.Unmarshal([]byte(dataLine), &meta)
					syncedOK <- meta.LastSeq
					close(ready)
				}
				if evName == "harness" && dataLine != "" {
					var ev sseEvent
					if err := json.Unmarshal([]byte(dataLine), &ev); err == nil {
						recvAt := time.Now().UnixMilli()
						mu.Lock()
						samples = append(samples, arrival{
							EventID: ev.EventID, EventType: ev.EventType, TaskID: ev.TaskID,
							Seq: ev.Seq, LagMs: float64(recvAt - ev.TimestampMs),
						})
						mu.Unlock()
					}
				}
				evName, dataLine = "", ""
			}
		}
	}()

	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		fatal("SSE catch-up did not finish in 30s")
	}
	lastSeq := <-syncedOK
	_ = lastSeq

	// 生产端：建任务必须发生在 SSE 就绪之后，否则早期事件会落在订阅之前，
	// 那条链路就不是「实时推送」而是「catch-up 回放」，口径不同。
	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fatal("dial %s: %v", *addr, err)
	}
	defer conn.Close()
	client := harnesspb.NewHarnessLoopServiceClient(conn)

	root, err := os.MkdirTemp("", "p2t1-events-*")
	if err != nil {
		fatal("temp dir: %v", err)
	}

	createStart := time.Now()
	var createdIDs []string
	for i := 0; i < *tasks; i++ {
		resp, err := client.CreateTask(ctx, &harnesspb.CreateTaskRequest{
			TenantId:      tenantID,
			ProjectId:     *project,
			WorkspaceRoot: fmt.Sprintf("%s/task-%02d", root, i),
			Goal:          fmt.Sprintf("events bench task %02d", i),
		})
		if err != nil {
			fatal("create task %d: %v", i, err)
		}
		id := resp.GetTask().GetTaskId()
		createdIDs = append(createdIDs, id)
	}

	// 等任务跑完（终态），保证事件已经全部产生。
	deadline := time.Now().Add(*wait)
	var firstEventAt time.Time
	for {
		done := 0
		for _, id := range createdIDs {
			v, err := client.GetTask(ctx, &harnesspb.GetTaskRequest{TenantId: tenantID, TaskId: id})
			if err == nil {
				switch v.GetStatus() {
				case "completed", "failed", "aborted":
					done++
				}
			}
		}
		mu.Lock()
		if firstEventAt.IsZero() && len(samples) > 0 {
			firstEventAt = time.Now()
		}
		mu.Unlock()
		if done == len(createdIDs) {
			break
		}
		if time.Now().After(deadline) {
			res.Notes = append(res.Notes, "timeout waiting for tasks to reach terminal state")
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	// 留一点余量让最后几个事件流出来（SSE 是流式写，不是事务提交即达）。
	time.Sleep(800 * time.Millisecond)

	mu.Lock()
	// seq 是租户级 global_seq：整路 SSE（不做 task 过滤）必须严格递增且逐条 +1。
	// 必须在校验完 raw 流之后再按 task_id 过滤——大屏订阅的是整个租户，子任务、
	// 别的任务的事件本来就会占号；用「只留本轮任务」的样本去判跳号会误报。
	for i := 1; i < len(samples); i++ {
		if samples[i].Seq <= samples[i-1].Seq {
			res.SeqMonotonic = false
		}
		if samples[i].Seq > samples[i-1].Seq+1 {
			res.SeqGaps++
		}
	}
	// 过滤只用 task_id：回放的历史事件与本轮无关，延迟也只能按本轮任务算。
	mine := make(map[string]bool, len(createdIDs))
	for _, id := range createdIDs {
		mine[id] = true
	}
	kept := samples[:0]
	for _, s := range samples {
		if mine[s.TaskID] {
			kept = append(kept, s)
		}
	}
	dropped += len(samples) - len(kept)
	allSamples := append([]arrival(nil), samples...)
	samples = kept

	res.Events = len(samples)
	res.Dropped = dropped
	res.Samples = append([]arrival(nil), samples...)
	for _, s := range samples {
		res.ByType[s.EventType]++
	}
	lags := make([]float64, 0, len(samples))
	for _, s := range samples {
		lags = append(lags, s.LagMs)
	}
	mu.Unlock()

	// 收集率的分母来自服务端数据库：本轮任务实际落库了多少条事件。
	uniq := make(map[string]bool, len(samples))
	for _, s := range samples {
		uniq[s.EventID] = true
	}
	if *dsn != "" {
		expected, countErr := countEvents(ctx, *dsn, tenantID, createdIDs)
		if countErr != nil {
			res.Notes = append(res.Notes, "collection rate unavailable: "+countErr.Error())
		} else {
			res.Expected = expected
			res.Missing = expected - len(uniq)
			if res.Missing < 0 {
				res.Missing = 0
			}
			if expected > 0 {
				res.Collected = float64(len(uniq)) / float64(expected)
			}
			res.Budgets["collection_rate_100"] = expected > 0 && len(uniq) >= expected
		}
	} else {
		res.Notes = append(res.Notes, "collection rate not measured: -dsn not set")
	}

	sort.Float64s(lags)
	res.P50Ms = pctF(lags, 50)
	res.P95Ms = pctF(lags, 95)
	res.P99Ms = pctF(lags, 99)
	if len(lags) > 0 {
		res.MaxMs = lags[len(lags)-1]
	}
	if !firstEventAt.IsZero() {
		res.FirstEventMs = firstEventAt.Sub(createStart).Milliseconds()
	}

	// 补拉校验：从倒数第 3 条之前重连一次，catch-up 必须正好回放最后 3 条、
	// global_seq 连续、不重不漏，且 synced 的 last_seq 推到当前最新。
	rawMax := int64(0)
	for _, s := range allSamples {
		if s.Seq > rawMax {
			rawMax = s.Seq
		}
	}
	if rawMax > 3 {
		from := rawMax - 3
		seqs, synced, probeErr := catchupProbe(ctx, streamBase, from)
		res.CatchupSeq = seqs
		if probeErr != nil {
			res.Notes = append(res.Notes, "catch-up probe failed: "+probeErr.Error())
		} else {
			ok := len(seqs) == 3 && synced == rawMax
			for i, s := range seqs {
				if s != from+int64(i)+1 {
					ok = false
				}
			}
			res.CatchupOK = ok
			res.Budgets["catchup_no_dup_no_gap"] = ok
			if !ok {
				res.Notes = append(res.Notes, fmt.Sprintf(
					"catch-up replayed %v from %d, synced=%d, want %d", seqs, from, synced, rawMax))
			}
		}
	}

	res.Hardware = map[string]any{
		"os":     "windows/amd64",
		"link":   "harnessloop-service -> gateway SSE -> client (loopback)",
		"budget": "DFX-P-07 P95 <= 1s (LAN)",
	}
	res.Budgets["p95_le_budget"] = res.P95Ms <= *budget
	res.Budgets["events_observed"] = res.Events > 0
	res.Budgets["seq_monotonic_no_gap"] = res.SeqMonotonic && res.SeqGaps == 0
	res.Passed = res.Budgets["p95_le_budget"] && res.Budgets["events_observed"]
	if rate, ok := res.Budgets["collection_rate_100"]; ok {
		res.Passed = res.Passed && rate
	}
	if ok, exists := res.Budgets["catchup_no_dup_no_gap"]; exists {
		res.Passed = res.Passed && ok
	}
	if res.Events == 0 {
		res.Notes = append(res.Notes, "no event carried a task_id created in this run; check tenant filtering")
	}

	emit(res, *out)
}

// catchupProbe 用指定的 last_seq 重连一次，返回 catch-up 阶段（synced 之前）
// 收到的事件 seq 与网关上报的 last_seq。
func catchupProbe(ctx context.Context, streamBase string, from int64) ([]int64, int64, error) {
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	url := fmt.Sprintf("%s&last_seq=%d", streamBase, from)
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("SSE status %d", resp.StatusCode)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var (
		evName, dataLine string
		seqs             []int64
	)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			evName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLine = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case line == "":
			if evName == "harness" && dataLine != "" {
				var ev sseEvent
				if err := json.Unmarshal([]byte(dataLine), &ev); err == nil {
					seqs = append(seqs, ev.Seq)
				}
			}
			if evName == "synced" {
				var meta struct {
					LastSeq int64 `json:"last_seq"`
				}
				_ = json.Unmarshal([]byte(dataLine), &meta)
				return seqs, meta.LastSeq, nil
			}
			evName, dataLine = "", ""
		}
	}
	if err := scanner.Err(); err != nil {
		return seqs, 0, err
	}
	return seqs, 0, fmt.Errorf("stream ended before synced")
}

func pctF(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p / 100)
	return sorted[idx]
}

func ctx0() context.Context { return context.Background() }

// countEvents 是收集率的分母：本轮任务在服务端实际落库的事件条数。
func countEvents(ctx context.Context, dsn, tenant string, taskIDs []string) (int, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close(ctx) }()
	var n int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM hlp_event.events WHERE tenant_id = $1 AND task_id = ANY($2)`,
		tenant, taskIDs).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// purgeEvents 清掉指定租户的历史事件，把大屏刚打开时的空库场景复现出来：
// 清空后 client 的游标停在 0，测到的才是「纯实时推送」，不会混入 catch-up 回放的
// 历史事件。T-P2-17 修好游标语义后这不再是绕开缺陷的手段，只是口径选择。
func purgeEvents(ctx context.Context, dsn, tenant string) (int64, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close(ctx) }()
	tag, err := conn.Exec(ctx, `DELETE FROM hlp_event.events WHERE tenant_id = $1`, tenant)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func emit(res result, out string) {
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		fatal("marshal: %v", err)
	}
	if out != "" {
		_ = os.WriteFile(out, b, 0o644)
	}
	fmt.Println(string(b))
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "events: "+format+"\n", args...)
	os.Exit(1)
}
