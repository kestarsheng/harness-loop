// events 是 T-P2-T1 的「事件端到端」压测程序（DFX-P-07）。
//
// 测的是「事件从服务端产生到 SSE 客户端收到」这一段真实链路：
//
//	harnessloop-service 写 hlp_event.events
//	  → gateway WatchEvents(gRPC stream) → MonitorStream(SSE) → 本程序
//
// 延迟 = 本地收到时刻 − 事件自带的 timestamp_ms。两端同机同钟，无时钟偏差。
//
// 一个坑必须绕开：SSE 连接建立时会先做 catch-up 回放历史事件（last_seq 之前
// 的全部事件），那些事件的 timestamp_ms 可能是几十分钟前，直接统计会把 P95
// 拉到分钟级，得到完全错误的结论。所以本程序只统计「本轮创建的任务」产生的
// 事件——用 task_id 集合过滤，而不是用时间窗猜。
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

	streamURL := fmt.Sprintf("%s/api/v2/harness-loop/monitor/stream?tenant=%s&last_seq=0", *base, tenantID)

	ctx, cancel := context.WithTimeout(context.Background(), *wait+60*time.Second)
	defer cancel()

	res := result{
		Name:      "events_e2e",
		StartedAt: time.Now().Format(time.RFC3339),
		Base:      *base,
		StreamURL: streamURL,
		Tasks:     *tasks,
		ByType:    map[string]int{},
		Budgets:   map[string]bool{},
	}

	// 收集端：先连 SSE，等 catch-up 结束（收到 synced）再算「就绪」。
	//
	// 过滤只能用 task_id，不能用 seq——这是被实测推翻的：
	// hlp_event.events 的 seq 是 per-task 自增（`SELECT MAX(seq) WHERE task_id`），
	// 不是租户级全局序列。新任务的第一个事件 seq 又是 1，拿 catch-up 的
	// last_seq 当下界会把本轮事件 100% 误判为历史（第一次跑就是这么丢光的）。
	//
	// 所以这里全收，最后按本轮 createdIDs 过一遍。catch-up 回放的历史事件
	// 天然属于别的 task_id，会被这道过滤摘掉。
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
	// 最终再按本轮 task_id 过一遍：seq 下界能排除回放，但排除不掉「同一租户
	// 上其它客户端刚写的事件」——压测机上没有别人，仍做这道过滤以防串味。
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

	res.Hardware = map[string]any{
		"os":     "windows/amd64",
		"link":   "harnessloop-service -> gateway SSE -> client (loopback)",
		"budget": "DFX-P-07 P95 <= 1s (LAN)",
	}
	res.Budgets["p95_le_budget"] = res.P95Ms <= *budget
	res.Budgets["events_observed"] = res.Events > 0
	res.Passed = res.Budgets["p95_le_budget"] && res.Budgets["events_observed"]
	if res.Events == 0 {
		res.Notes = append(res.Notes, "no event carried a task_id created in this run; check tenant filtering")
	}

	emit(res, *out)
}

func pctF(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p / 100)
	return sorted[idx]
}

func ctx0() context.Context { return context.Background() }

// purgeEvents 清掉指定租户的历史事件，等于把大屏刚打开时的空库场景复现出来。
//
// 为什么必须清：网关 SSE 的 catch-up 会把 since 推到历史最大 seq，
// 而 hlp_event.events.seq 是 per-task 自增，新任务又是从 1 开始，
// 于是 WatchFrom 的 `seq > since` 会把新任务的事件永久滤掉（详见报告偏差项）。
// 清空后 since 停在 0，测到的才是真实推送延迟。
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
