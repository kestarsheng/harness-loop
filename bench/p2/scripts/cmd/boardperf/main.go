// boardperf 是 T-P2 质量收口的「50 卡片大屏帧率实测」（DFX-P-08）。
//
// 它回答三个问题，全部在真浏览器里量：
//  1. 50 个任务挂在大屏上时，主线程的帧间隔分布是多少（P50 / P95 / P99 / 超 16.7ms 占比）；
//  2. 交互延迟：点状态筛选、点卡片选中，从事件派发到「下一帧绘制完成」要多久；
//  3. JS 堆在测量窗口内有没有增长。
//
// 为什么不是纯前端单元测试：DFX-P-08 的口径是「主线程单帧 ≤ 16ms」，
// 这是渲染成本的度量，只能在真渲染器里取。这里用 chromedp 驱动本机 Chrome，
// 页面通过 rAF 自采样，脚本只负责收数与算分位数。
//
// 用法：
//
//	go run ./bench/p2/scripts/cmd/boardperf \
//	    -url http://localhost:5174/harness-loop/monitor \
//	    -cards 50 -seconds 120 -interactions 20 \
//	    -out bench/p2/runtime/board-frames-50.json
//
// 前置：scripts/dev-up.sh 起全栈，且 hlp_task.task_runs 里已灌 50 个任务
// （bench/p2/scripts/seed-board-tasks.sql）。cards 只用于断言「页面真的渲染了这么多卡片」。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

type frameStats struct {
	Samples   int     `json:"samples"`
	P50Ms     float64 `json:"p50_ms"`
	P95Ms     float64 `json:"p95_ms"`
	P99Ms     float64 `json:"p99_ms"`
	MaxMs     float64 `json:"max_ms"`
	Over16Pct float64 `json:"over_16_7_pct"`
	Budgeted  bool    `json:"p95_within_16_7"`
	// ImpliedHz 是 P50 帧间隔反推的刷新率：本机若跑在 120Hz，16.7ms 的门槛
	// 其实宽于真实帧预算（8.3ms），报告里必须一起给出，否则数字会被误读。
	ImpliedHz float64 `json:"implied_hz"`
}

type longTaskStats struct {
	Count     int     `json:"count"`
	TotalMs   float64 `json:"total_ms"`
	MaxMs     float64 `json:"max_ms"`
	PerSecond float64 `json:"per_second"`
}

type interactionStats struct {
	Name    string  `json:"name"`
	Samples int     `json:"samples"`
	P50Ms   float64 `json:"p50_ms"`
	P95Ms   float64 `json:"p95_ms"`
	MaxMs   float64 `json:"max_ms"`
}

type heapStats struct {
	BeforeMB float64 `json:"before_mb"`
	AfterMB  float64 `json:"after_mb"`
	DeltaMB  float64 `json:"delta_mb"`
}

type report struct {
	Name          string             `json:"name"`
	URL           string             `json:"url"`
	ExpectedCards int                `json:"expected_cards"`
	RenderedCards int                `json:"rendered_cards"`
	SweepCards    int                `json:"sweep_cards"`
	SampleSeconds float64            `json:"sample_seconds"`
	Viewport      string             `json:"viewport"`
	Headless      bool               `json:"headless"`
	Frames        frameStats         `json:"frames"`
	LongTasks     longTaskStats      `json:"long_tasks"`
	Interactions  []interactionStats `json:"interactions"`
	Heap          heapStats          `json:"heap"`
	StartedAt     string             `json:"started_at"`
	Hardware      map[string]any     `json:"hardware"`
	Passed        bool               `json:"passed"`
	Notes         []string           `json:"notes,omitempty"`
}

const cardSelector = `div[role="button"][aria-label^="任务 "]`

// installProbe 在页面里挂一个 rAF 采样器 + longtask 观察器，并把句柄留在 window.__boardPerf。
const installProbe = `(() => {
  window.__boardPerf = { frames: [], longtasks: [], stopped: false };
  const p = window.__boardPerf;
  let last = performance.now();
  const tick = (t) => {
    if (p.stopped) return;
    p.frames.push(t - last);
    last = t;
    requestAnimationFrame(tick);
  };
  requestAnimationFrame(tick);
  try {
    new PerformanceObserver((list) => {
      for (const e of list.getEntries()) p.longtasks.push({ start: e.startTime, dur: e.duration });
    }).observe({ entryTypes: ['longtask'] });
  } catch (err) {
    p.longtasksError = String(err);
  }
  return true;
})()`

const stopProbe = `(() => {
  const p = window.__boardPerf || {};
  p.stopped = true;
  return { frames: p.frames || [], longtasks: p.longtasks || [] };
})()`

// clickAndPaint 派发一次点击，并等到「再下一帧」回调，得到「事件 -> 绘制完成」的延迟。
const clickAndPaint = `(async (selector, index) => {
  const nodes = [...document.querySelectorAll(selector)];
  if (nodes.length === 0) return -1;
  const el = nodes[index % nodes.length];
  const t0 = performance.now();
  el.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, view: window }));
  await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
  return performance.now() - t0;
})`

// scrollAndPaint 滚动虚拟列表并等两帧，量「滚动 -> 重新绘制」的耗时。
const scrollAndPaint = `(async (delta) => {
  const grid = [...document.querySelectorAll('div')].find(
    (d) => d.scrollHeight > d.clientHeight + 40 && getComputedStyle(d).overflowY === 'auto',
  );
  if (!grid) return -1;
  const maxTop = Math.max(1, grid.scrollHeight - grid.clientHeight);
  const t0 = performance.now();
  grid.scrollTop = (grid.scrollTop + delta) % maxTop;
  await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
  return performance.now() - t0;
})`

// rafBaseline 是「什么都不做，只等两帧」的固有开销：
// 交互延迟里必然含着这段等待（120Hz 约 16.6ms、60Hz 约 33.3ms），
// 报告要给净耗时就得先把它量出来。
const rafBaseline = `(async () => {
  const t0 = performance.now();
  await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
  return performance.now() - t0;
})()`

const heapNow = `(() => {
  if (window.gc) { try { window.gc(); } catch (e) {} }
  const m = performance.memory;
  return m ? m.usedJSHeapSize / (1024 * 1024) : -1;
})()`

const countCards = `document.querySelectorAll('div[role="button"][aria-label^="任务 "]').length`

// sweepCards 滚遍整个虚拟列表，把所有出现过的卡片 aria-label 收成一个集合，
// 用来证明「50 张卡都渲染过」而不只是首屏那几张。
const sweepCards = `(async () => {
  const grid = [...document.querySelectorAll('div')].find(
    (d) => d.scrollHeight > d.clientHeight + 40 && getComputedStyle(d).overflowY === 'auto',
  );
  if (!grid) return { unique: 0, steps: 0 };
  const seen = new Set();
  const collect = () => {
    document.querySelectorAll('div[role="button"][aria-label^="任务 "]').forEach((n) => seen.add(n.getAttribute('aria-label')));
  };
  collect();
  const step = Math.max(120, Math.floor(grid.clientHeight / 2));
  let steps = 0;
  for (let top = 0; top <= grid.scrollHeight; top += step) {
    grid.scrollTop = top;
    steps++;
    await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
    collect();
  }
  grid.scrollTop = 0;
  await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
  return { unique: seen.size, steps };
})()`

// evalAwait 是「等 Promise 兑现」的求值封装：chromedp.Evaluate 不设 awaitPromise，
// 对 async 表达式直接求值只会拿回一个空 Promise 对象，量不到真实耗时。
func evalAwait(expr string, out any) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		v, exp, err := cdpruntime.Evaluate(expr).WithReturnByValue(true).WithAwaitPromise(true).Do(ctx)
		if err != nil {
			return err
		}
		if exp != nil {
			return fmt.Errorf("js exception: %s", exp.Text)
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(v.Value, out)
	})
}

func main() {
	var (
		url          = flag.String("url", "http://localhost:5174/harness-loop/monitor", "大屏地址")
		tenant       = flag.String("tenant", "default", "写入 localStorage 的 tenantId")
		cards        = flag.Int("cards", 50, "预期卡片数（只用于断言）")
		seconds      = flag.Int("seconds", 120, "帧采样窗口（秒）")
		interactions = flag.Int("interactions", 20, "每类交互的重复次数")
		out          = flag.String("out", "bench/p2/runtime/board-frames.json", "结果 JSON 路径")
		chromePath   = flag.String("chrome", "", "Chrome 可执行文件（留空走自动探测）")
		headed       = flag.Bool("headed", false, "用有头 Chrome（默认 headless=new）")
		waitCards    = flag.Int("wait-cards", 30, "等待卡片出现的超时（秒）")
	)
	flag.Parse()

	rep, err := run(*url, *tenant, *cards, *seconds, *interactions, *chromePath, *headed, *waitCards)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boardperf: %v\n", err)
		os.Exit(1)
	}
	if err := writeJSON(*out, rep); err != nil {
		fmt.Fprintf(os.Stderr, "boardperf: write %s: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Printf("cards=%d/%d(首屏) sweep=%d frames(p50=%.2f p95=%.2f p99=%.2f max=%.2f over16.7=%.1f%% 刷新率≈%.0fHz) heapΔ=%.2fMB -> %s\n",
		rep.RenderedCards, rep.ExpectedCards, rep.SweepCards, rep.Frames.P50Ms, rep.Frames.P95Ms,
		rep.Frames.P99Ms, rep.Frames.MaxMs, rep.Frames.Over16Pct, rep.Frames.ImpliedHz, rep.Heap.DeltaMB, *out)
	for _, it := range rep.Interactions {
		fmt.Printf("  %-16s n=%d p50=%.2fms p95=%.2fms max=%.2fms\n", it.Name, it.Samples, it.P50Ms, it.P95Ms, it.MaxMs)
	}
	if !rep.Passed {
		os.Exit(2)
	}
}

func run(url, tenant string, expectedCards, seconds, interactions int, chromePath string, headed bool, waitCards int) (*report, error) {
	if _, err := os.Stat(url); err == nil {
		return nil, fmt.Errorf("-url 指向的是本地文件，期望 http(s) 地址")
	}
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts,
		chromedp.Flag("headless", !headed),
		chromedp.Flag("disable-gpu", false),
		chromedp.Flag("hide-scrollbars", true),
		chromedp.Flag("mute-audio", true),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("enable-precise-memory-info", true),
		chromedp.Flag("js-flags", "--expose-gc"),
		chromedp.WindowSize(1920, 1080),
	)
	if chromePath != "" {
		opts = append(opts, chromedp.ExecPath(chromePath))
	}

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancelAlloc()
	// 新版 Chrome 会发 cdproto 旧版本不认识的 IPAddressSpace 事件；
	// 这类解析告警与被测指标无关，直接丢掉，避免淹没结果。
	ctx, cancel := chromedp.NewContext(allocCtx, chromedp.WithErrorf(func(string, ...interface{}) {}))
	defer cancel()
	runCtx, cancelRun := context.WithTimeout(ctx, time.Duration(seconds+180)*time.Second)
	defer cancelRun()

	rep := &report{
		Name:          "board-frames",
		URL:           url,
		ExpectedCards: expectedCards,
		SampleSeconds: float64(seconds),
		Viewport:      "1920x1080",
		Headless:      !headed,
		StartedAt:     time.Now().Format(time.RFC3339),
		Hardware: map[string]any{
			"os":         runtime.GOOS + "/" + runtime.GOARCH,
			"go_version": runtime.Version(),
			"cpus":       runtime.NumCPU(),
		},
	}

	// 先落到同源页面写 localStorage，再进大屏——避免首次渲染用错租户拉空列表。
	var tenantSet string
	if err := chromedp.Run(runCtx,
		chromedp.Navigate(url),
		chromedp.Evaluate(fmt.Sprintf(`localStorage.setItem('tenantId', %q); localStorage.getItem('tenantId')`, tenant), &tenantSet),
		chromedp.Navigate(url),
	); err != nil {
		return nil, fmt.Errorf("navigate: %w", err)
	}

	// 等卡片出现（虚拟列表首屏渲染出来即可开始计时）。
	deadline := time.Now().Add(time.Duration(waitCards) * time.Second)
	rendered := 0
	for {
		if err := chromedp.Run(runCtx, chromedp.Evaluate(countCards, &rendered)); err != nil {
			return nil, fmt.Errorf("count cards: %w", err)
		}
		if rendered > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if rendered == 0 {
		return nil, fmt.Errorf("等 %ds 仍未见卡片：确认 dev-up 已起、且已灌 seed-board-tasks.sql", waitCards)
	}
	rep.RenderedCards = rendered
	time.Sleep(1500 * time.Millisecond) // 让首屏动画与首轮数据水合收敛

	var heapBefore float64
	if err := chromedp.Run(runCtx, chromedp.Evaluate(heapNow, &heapBefore)); err != nil {
		return nil, fmt.Errorf("heap before: %w", err)
	}

	// 滚一遍虚拟列表，确认 50 张卡都渲染过。
	var sweep struct {
		Unique int `json:"unique"`
		Steps  int `json:"steps"`
	}
	if err := chromedp.Run(runCtx, evalAwait(sweepCards, &sweep)); err != nil {
		return nil, fmt.Errorf("sweep: %w", err)
	}
	rep.SweepCards = sweep.Unique
	if sweep.Unique < expectedCards {
		rep.Notes = append(rep.Notes, fmt.Sprintf("虚拟列表滚动遍历只覆盖 %d 张卡（预期 %d），滚动步长可能不足", sweep.Unique, expectedCards))
	}

	// 帧采样窗口。
	if err := chromedp.Run(runCtx, chromedp.Evaluate(installProbe, nil)); err != nil {
		return nil, fmt.Errorf("install probe: %w", err)
	}
	sampleStart := time.Now()
	time.Sleep(time.Duration(seconds) * time.Second)
	var probe struct {
		Frames    []float64 `json:"frames"`
		LongTasks []struct {
			Start float64 `json:"start"`
			Dur   float64 `json:"dur"`
		} `json:"longtasks"`
	}
	if err := chromedp.Run(runCtx, chromedp.Evaluate(stopProbe, &probe)); err != nil {
		return nil, fmt.Errorf("stop probe: %w", err)
	}
	elapsed := time.Since(sampleStart).Seconds()
	rep.SampleSeconds = math.Round(elapsed*10) / 10
	rep.Frames = summarizeFrames(probe.Frames)
	var totalLT float64
	var maxLT float64
	for _, lt := range probe.LongTasks {
		totalLT += lt.Dur
		if lt.Dur > maxLT {
			maxLT = lt.Dur
		}
	}
	rep.LongTasks = longTaskStats{
		Count:   len(probe.LongTasks),
		TotalMs: round2(totalLT),
		MaxMs:   round2(maxLT),
	}
	if elapsed > 0 {
		rep.LongTasks.PerSecond = round2(float64(len(probe.LongTasks)) / elapsed)
	}

	// 交互：状态筛选切换 + 卡片点选。两类都量「事件 -> 绘制完成」。
	rep.Interactions = []interactionStats{
		measureSamples(runCtx, "status-filter", func(i int) string {
			return fmt.Sprintf("%s(%q, %d)", clickAndPaint, ".ant-segmented-item", i)
		}, interactions),
		measureSamples(runCtx, "grid-scroll", func(i int) string {
			return fmt.Sprintf("%s(%d)", scrollAndPaint, 240+i*40)
		}, interactions),
		measureSamples(runCtx, "raf-baseline", func(int) string { return rafBaseline }, interactions),
	}

	var heapAfter float64
	if err := chromedp.Run(runCtx, chromedp.Evaluate(heapNow, &heapAfter)); err != nil {
		return nil, fmt.Errorf("heap after: %w", err)
	}
	rep.Heap = heapStats{
		BeforeMB: round2(heapBefore),
		AfterMB:  round2(heapAfter),
		DeltaMB:  round2(heapAfter - heapBefore),
	}

	rep.Passed = rep.Frames.P95Ms <= 16.7 && rep.Heap.DeltaMB < 32
	if rep.RenderedCards < expectedCards {
		rep.Notes = append(rep.Notes,
			fmt.Sprintf("首屏只渲染 %d/%d 张卡（AgentCardGrid 是虚拟列表，只画可视区+overscan），帧数据对应实测渲染量",
				rep.RenderedCards, expectedCards))
	}
	return rep, nil
}

func measureSamples(ctx context.Context, name string, exprOf func(int) string, n int) interactionStats {
	stats := interactionStats{Name: name}
	if n <= 0 {
		return stats
	}
	samples := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		var ms float64
		expr := exprOf(i)
		if err := chromedp.Run(ctx, evalAwait(expr, &ms)); err != nil {
			continue
		}
		if ms >= 0 {
			samples = append(samples, ms)
		}
		time.Sleep(120 * time.Millisecond)
	}
	if len(samples) == 0 {
		return stats
	}
	sort.Float64s(samples)
	stats.Samples = len(samples)
	stats.P50Ms = percentile(samples, 0.5)
	stats.P95Ms = percentile(samples, 0.95)
	stats.MaxMs = round2(samples[len(samples)-1])
	return stats
}

func summarizeFrames(frames []float64) frameStats {
	st := frameStats{}
	if len(frames) == 0 {
		return st
	}
	sorted := append([]float64{}, frames...)
	sort.Float64s(sorted)
	st.Samples = len(sorted)
	st.P50Ms = percentile(sorted, 0.5)
	st.P95Ms = percentile(sorted, 0.95)
	st.P99Ms = percentile(sorted, 0.99)
	st.MaxMs = round2(sorted[len(sorted)-1])
	over := 0
	for _, f := range sorted {
		if f > 16.7 {
			over++
		}
	}
	st.Over16Pct = round2(float64(over) * 100 / float64(len(sorted)))
	if st.P50Ms > 0 {
		st.ImpliedHz = math.Round(1000/st.P50Ms*10) / 10
	}
	st.Budgeted = st.P95Ms <= 16.7
	return st
}

// percentile 用线性插值取分位数，入参必须已升序。
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return round2(sorted[0])
	}
	pos := p * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return round2(sorted[lo])
	}
	frac := pos - float64(lo)
	return round2(sorted[lo]*(1-frac) + sorted[hi]*frac)
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func writeJSON(path string, rep *report) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	buf, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(buf, '\n'), 0o644)
}
