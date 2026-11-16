// memload 是 T-P2-T1 的「20 并发任务常驻内存」压测程序（DFX-P-09）。
//
// 它回答两个问题，都用同一台机器上的真实进程数为证：
//  1. 并发 20 个任务时，harnessloop-service 进程 RSS 峰值是否 ≤ 4GB；
//  2. 摊到单任务上的增量内存是否 ≤ 256MB。
//
// 为什么用 tasklist 而不是进程内 runtime.MemStats：
// 被测进程不是我，MemStats 拿不到；RSS 才是 DFX-P-09 的口径（含 Go 堆以外的映射）。
// 为避免给 go.mod 增加依赖（本任务文件域不含 go.mod），Windows 侧直接调 tasklist，
// 其余平台退回 /proc/<pid>/status。
//
// 用法：
//
//	go run ./bench/p2/scripts/cmd/memload -n 20 -addr 127.0.0.1:50008
//	go run ./bench/p2/scripts/cmd/memload -n 20 -pid 12345 -out mem.json
//
// 前置：harnessloop-service 已按 bench/p2/scripts/runtime/harnessloop.yaml 启动，
// scripts/fakeagent 监听 127.0.0.1:50002（不接真实 LLM，保证可复现）。
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	harnesspb "agent-platform/pkg/pb/harnessloop"
)

type sample struct {
	AtMs      int64   `json:"at_ms"`
	RSSMB     float64 `json:"rss_mb"`
	Completed int     `json:"completed"`
	Running   int     `json:"running"`
}

type taskOutcome struct {
	TaskID    string `json:"task_id"`
	Status    string `json:"status"`
	Reason    string `json:"reason"`
	CreateMs  int64  `json:"create_ms"`
	FinishMs  int64  `json:"finish_ms"`
	Stage     string `json:"final_stage"`
	ErrorText string `json:"error,omitempty"`
}

type result struct {
	Name        string          `json:"name"`
	StartedAt   string          `json:"started_at"`
	Addr        string          `json:"addr"`
	Tenant      string          `json:"tenant"`
	Project     string          `json:"project"`
	Concurrency int             `json:"concurrency"`
	PID         int             `json:"pid"`
	BaselineMB  float64         `json:"baseline_rss_mb"`
	PeakMB      float64         `json:"peak_rss_mb"`
	FinalMB     float64         `json:"final_rss_mb"`
	PerTaskMB   float64         `json:"per_task_mb"`
	CreateP95Ms int64           `json:"create_p95_ms"`
	AllDoneMs   int64           `json:"all_done_ms"`
	Completed   int             `json:"completed"`
	Failed      int             `json:"failed"`
	TimedOut    int             `json:"timed_out"`
	Samples     []sample        `json:"samples"`
	Tasks       []taskOutcome   `json:"tasks"`
	Budgets     map[string]bool `json:"budgets"`
	Hardware    map[string]any  `json:"hardware"`
	Passed      bool            `json:"passed"`
	Notes       []string        `json:"notes"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:50008", "harnessloop-service gRPC address")
	n := flag.Int("n", 20, "concurrent tasks")
	tenant := flag.String("tenant", "tenant-bench-p2t1", "tenant id")
	project := flag.String("project", "proj-bench-p2t1", "project id")
	pid := flag.Int("pid", 0, "harnessloop-service PID (0 = resolve by process name)")
	procName := flag.String("proc", "harnessloop-service", "process name used to resolve PID")
	interval := flag.Duration("interval", 200*time.Millisecond, "RSS sampling interval")
	timeout := flag.Duration("timeout", 180*time.Second, "waiting budget for all tasks")
	workspace := flag.String("workspace", "", "workspace root shared by tasks (default: a temp dir per run)")
	out := flag.String("out", "", "write JSON result here")
	flag.Parse()

	if *pid == 0 {
		p, err := resolvePID(*procName)
		if err != nil {
			fatal("resolve pid: %v", err)
		}
		*pid = p
	}

	root := *workspace
	if root == "" {
		dir, err := os.MkdirTemp("", "p2t1-memload-*")
		if err != nil {
			fatal("temp dir: %v", err)
		}
		root = dir
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout+60*time.Second)
	defer cancel()

	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fatal("dial %s: %v", *addr, err)
	}
	defer conn.Close()
	client := harnesspb.NewHarnessLoopServiceClient(conn)

	res := result{
		Name:        "memload_20",
		StartedAt:   time.Now().Format(time.RFC3339),
		Addr:        *addr,
		Tenant:      *tenant,
		Project:     *project,
		Concurrency: *n,
		PID:         *pid,
		Budgets:     map[string]bool{},
	}

	// 基线取 3 次中位数：单次采样会被 GC 时点带偏。
	base, err := medianRSS(*pid, 3, 150*time.Millisecond)
	if err != nil {
		fatal("baseline rss: %v", err)
	}
	res.BaselineMB = base

	start := time.Now()
	var wg sync.WaitGroup
	outcomes := make([]taskOutcome, *n)
	createLat := make([]int64, *n)

	for i := 0; i < *n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			t0 := time.Now()
			resp, err := client.CreateTask(ctx, &harnesspb.CreateTaskRequest{
				TenantId:      *tenant,
				ProjectId:     *project,
				WorkspaceRoot: filepath.Join(root, fmt.Sprintf("task-%02d", idx)),
				Goal:          fmt.Sprintf("memload task %02d: exercise the five-stage loop", idx),
			})
			createLat[idx] = time.Since(t0).Milliseconds()
			if err != nil {
				outcomes[idx] = taskOutcome{ErrorText: err.Error(), Status: "create_failed"}
				return
			}
			outcomes[idx] = taskOutcome{TaskID: resp.GetTask().GetTaskId(), Status: resp.GetTask().GetStatus(), CreateMs: createLat[idx]}
		}(i)
	}
	wg.Wait()

	created := 0
	for _, o := range outcomes {
		if o.TaskID != "" {
			created++
		}
	}
	if created == 0 {
		fatal("no task was created; check harnessloop-service logs")
	}

	// 采样循环：每 interval 采一次 RSS 与任务状态，直到全部终态或超时。
	deadline := time.Now().Add(*timeout)
	lastProgress := time.Now()
	for {
		time.Sleep(*interval)
		rss, err := readRSS(*pid)
		if err != nil {
			continue
		}
		done, running, failed := 0, 0, 0
		// 并发查状态：20 个任务串行 GetTask 会让每轮循环多花几百毫秒，
		// 采样点被拉稀到看不出峰值。并发查完再统计。
		type statusPair struct {
			idx  int
			view *harnesspb.TaskView
			err  error
		}
		ch := make(chan statusPair, created)
		var sg sync.WaitGroup
		for i := range outcomes {
			if outcomes[i].TaskID == "" {
				continue
			}
			sg.Add(1)
			go func(idx int) {
				defer sg.Done()
				v, err := client.GetTask(ctx, &harnesspb.GetTaskRequest{TenantId: *tenant, TaskId: outcomes[idx].TaskID})
				ch <- statusPair{idx: idx, view: v, err: err}
			}(i)
		}
		sg.Wait()
		close(ch)
		for p := range ch {
			if p.err != nil || p.view == nil {
				continue
			}
			st := p.view.GetStatus()
			if outcomes[p.idx].FinishMs == 0 && (st == "completed" || st == "failed" || st == "aborted") {
				outcomes[p.idx].FinishMs = time.Since(start).Milliseconds()
				outcomes[p.idx].Status = st
				outcomes[p.idx].Reason = p.view.GetTerminalReason()
				outcomes[p.idx].Stage = p.view.GetCurrentStage()
			}
			switch st {
			case "completed":
				done++
			case "failed", "aborted":
				failed++
			default:
				running++
			}
		}
		res.Samples = append(res.Samples, sample{
			AtMs:      time.Since(start).Milliseconds(),
			RSSMB:     rss,
			Completed: done,
			Running:   running,
		})
		if rss > res.PeakMB {
			res.PeakMB = rss
		}
		if done+failed >= created {
			lastProgress = time.Now()
			break
		}
		if time.Now().After(deadline) {
			res.Notes = append(res.Notes, fmt.Sprintf("timeout after %s: %d/%d terminal", *timeout, done+failed, created))
			break
		}
		_ = lastProgress
	}

	res.AllDoneMs = time.Since(start).Milliseconds()
	res.FinalMB, _ = readRSS(*pid)
	res.Tasks = outcomes
	lat := append([]int64(nil), createLat...)
	res.CreateP95Ms = percentile(lat, 95)

	for _, o := range outcomes {
		switch o.Status {
		case "completed":
			res.Completed++
		case "failed", "aborted":
			res.Failed++
		default:
			res.TimedOut++
		}
	}

	// 峰值相对基线的增量才是「20 个任务带来的内存」，摊到单任务上比较才有意义。
	if res.PeakMB > res.BaselineMB && created > 0 {
		res.PerTaskMB = (res.PeakMB - res.BaselineMB) / float64(created)
	}

	res.Hardware = map[string]any{
		"os":             runtime.GOOS + "/" + runtime.GOARCH,
		"go_version":     runtime.Version(),
		"gomaxprocs":     runtime.GOMAXPROCS(0),
		"cores":          runtime.NumCPU(),
		"sample_ms":      interval.Milliseconds(),
		"deps":           "harnessloop-service + scripts/fakeagent (no real LLM)",
		"workspace_root": root,
	}
	res.Budgets["rss_le_4gb"] = res.PeakMB <= 4096
	res.Budgets["per_task_le_256mb"] = res.PerTaskMB <= 256
	res.Budgets["all_tasks_terminal"] = created == res.Completed+res.Failed
	res.Passed = res.Budgets["rss_le_4gb"] && res.Budgets["per_task_le_256mb"]

	emit(res, *out)
}

// resolvePID 按进程名找 PID。同名多进程时取第一个并提示。
func resolvePID(name string) (int, error) {
	switch runtime.GOOS {
	case "windows":
		outBytes, err := exec.Command("tasklist", "/FI", "IMAGENAME eq "+name+".exe", "/FO", "CSV", "/NH").Output()
		if err != nil {
			return 0, err
		}
		rows := parseCSV(string(outBytes))
		if len(rows) == 0 {
			return 0, fmt.Errorf("process %q.exe not found", name)
		}
		return strconv.Atoi(strings.TrimSpace(rows[0][1]))
	default:
		outBytes, err := exec.Command("pgrep", "-f", name).Output()
		if err != nil {
			return 0, err
		}
		fields := strings.Fields(string(outBytes))
		if len(fields) == 0 {
			return 0, fmt.Errorf("process %q not found", name)
		}
		return strconv.Atoi(fields[0])
	}
}

// readRSS 的平台实现见 rss_windows.go / rss_other.go：
// Windows 走 psapi（微秒级），其余平台读 /proc。

func parseCSV(s string) [][]string {
	r := csv.NewReader(strings.NewReader(strings.TrimSpace(s)))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		return nil
	}
	// tasklist 找不到进程时会回一句 "INFO: No tasks..."，不是 CSV。
	var out [][]string
	for _, row := range rows {
		if len(row) >= 5 {
			out = append(out, row)
		}
	}
	return out
}

func medianRSS(pid, n int, gap time.Duration) (float64, error) {
	var vals []float64
	for i := 0; i < n; i++ {
		v, err := readRSS(pid)
		if err == nil {
			vals = append(vals, v)
		}
		time.Sleep(gap)
	}
	if len(vals) == 0 {
		return 0, fmt.Errorf("no rss samples")
	}
	return percentileF(vals, 50), nil
}

func percentile(vals []int64, p float64) int64 {
	if len(vals) == 0 {
		return 0
	}
	cp := append([]int64(nil), vals...)
	sortInt64(cp)
	idx := int(float64(len(cp)-1) * p / 100)
	return cp[idx]
}

func percentileF(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	cp := append([]float64(nil), vals...)
	for i := 1; i < len(cp); i++ {
		for j := i; j > 0 && cp[j] < cp[j-1]; j-- {
			cp[j], cp[j-1] = cp[j-1], cp[j]
		}
	}
	idx := int(float64(len(cp)-1) * p / 100)
	return cp[idx]
}

func sortInt64(v []int64) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

func emit(res result, out string) {
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		fatal("marshal: %v", err)
	}
	if out != "" {
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err == nil {
			_ = os.WriteFile(out, b, 0o644)
		}
	}
	fmt.Println(string(b))
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "memload: "+format+"\n", args...)
	os.Exit(1)
}
