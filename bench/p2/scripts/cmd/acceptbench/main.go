// acceptbench 是 T-P2-T1 的「阶段验收」压测程序（DFX-P-06）。
//
// 测的是确定性检查路径：Assemble(Input) → ProgramStageVerifier.Verify →
// checks.Registry 逐条判据执行子进程 → Verdict。全程不调模型，
// 对应 design 9 的「全部可程序化 → 只跑程序检查」那条 10s 路径。
//
// 判据两条，一条轻一条重，故意不只用 FileExists：
//
//	artifact_exists(main.go) —— 走文件系统，微秒级
//	build_pass              —— 真的起 go build 子进程，秒级
//
// 只用 FileExists 会把 P95 压到 1ms 以下，看起来漂亮，但没测到验收真正的成本。
//
// 用法：
//
//	go run ./bench/p2/scripts/cmd/acceptbench -n 20
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"agent-platform/pkg/harnessloop/acceptance"
	"agent-platform/pkg/harnessloop/acceptance/checks"
)

type result struct {
	Name       string          `json:"name"`
	StartedAt  string          `json:"started_at"`
	Iterations int             `json:"iterations"`
	WorkDir    string          `json:"workdir"`
	Criteria   []string        `json:"criteria"`
	P50Ms      float64         `json:"p50_ms"`
	P95Ms      float64         `json:"p95_ms"`
	P99Ms      float64         `json:"p99_ms"`
	MaxMs      float64         `json:"max_ms"`
	Verdicts   map[string]int  `json:"verdicts"`
	Errors     int             `json:"errors"`
	Budgets    map[string]bool `json:"budgets"`
	Hardware   map[string]any  `json:"hardware"`
	Passed     bool            `json:"passed"`
	Notes      []string        `json:"notes"`
}

func main() {
	workdir := flag.String("workdir", filepath.Join("bench", "p2", "scripts", "testdata", "acceptfix"), "fixture workspace")
	n := flag.Int("n", 20, "iterations")
	light := flag.Bool("light", false, "only run in-process criteria (no go build subprocess) — used to attribute latency")
	budgetMs := flag.Float64("budget-ms", 10000, "P95 budget (DFX-P-06 deterministic path)")
	out := flag.String("out", "", "write JSON result here")
	flag.Parse()

	abs, err := filepath.Abs(*workdir)
	if err != nil {
		fatal("abs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(abs, "go.mod")); err != nil {
		fatal("workdir %s has no go.mod (build_pass requires one)", abs)
	}

	criteria := []acceptance.Criterion{
		{ID: "artifact-present", Type: "artifact_exists", Expr: "main.go", Required: true},
		{ID: "build-passes", Type: "build_pass", Expr: "build ./...", Required: true},
	}
	criteriaDesc := []string{"artifact_exists:main.go", "build_pass:build ./..."}
	if *light {
		// 只留进程内判据：用来回答「超标的到底是验收编排还是 go 子进程」。
		criteria = criteria[:1]
		criteriaDesc = []string{"artifact_exists:main.go"}
	}

	verifier := acceptance.NewProgramStageVerifier(checks.NewRegistry())
	ctx := context.Background()

	// GoalHash 是 Input.Validate 的必填项；用目标文本的 sha256，保持可复现。
	goalSum := sha256.Sum256([]byte("produce a buildable workspace"))
	goalHash := "sha256:" + hex.EncodeToString(goalSum[:])

	res := result{
		Name:       "acceptance_program",
		StartedAt:  time.Now().Format(time.RFC3339),
		Iterations: *n,
		WorkDir:    abs,
		Criteria:   criteriaDesc,
		Verdicts:   map[string]int{},
		Budgets:    map[string]bool{},
	}

	var lat []float64
	for i := 0; i < *n; i++ {
		input, err := acceptance.Assemble(acceptance.Spec{
			AcceptanceID: fmt.Sprintf("acc-bench-%02d", i),
			TaskID:       "task-bench-p2t1",
			TenantID:     "tenant-bench-p2t1",
			ProjectID:    "proj-bench-p2t1",
			Stage:        "implementation",
			Goal:         "produce a buildable workspace",
			GoalHash:     goalHash,
			Criteria:     criteria,
			Constraints: acceptance.Constraints{
				ToolWhitelist: []string{"read_file", "run_command"},
				WorkspaceRoot: abs,
			},
		})
		if err != nil {
			res.Errors++
			res.Notes = append(res.Notes, fmt.Sprintf("assemble %d: %v", i, err))
			continue
		}
		t0 := time.Now()
		verdict, err := verifier.Verify(ctx, acceptance.StageVerifyRequest{
			Input:    input,
			Criteria: criteria,
			Channel:  "program",
			WorkDir:  abs,
			Timeout:  60 * time.Second,
		})
		el := float64(time.Since(t0).Microseconds()) / 1000
		if err != nil {
			res.Errors++
			res.Notes = append(res.Notes, fmt.Sprintf("verify %d: %v", i, err))
			continue
		}
		lat = append(lat, el)
		res.Verdicts[string(verdict.Verdict)]++
	}

	if len(lat) == 0 {
		for _, note := range res.Notes {
			fmt.Fprintln(os.Stderr, "  note:", note)
		}
		fatal("no successful verification round")
	}
	sort.Float64s(lat)
	res.P50Ms = pctF(lat, 50)
	res.P95Ms = pctF(lat, 95)
	res.P99Ms = pctF(lat, 99)
	res.MaxMs = lat[len(lat)-1]

	res.Hardware = map[string]any{
		"os":      "windows/amd64",
		"channel": "program-only (no LLM judge), per DFX-P-06 fast path",
		"budget":  "P95 <= 10s (deterministic) / 60s (with model)",
		"fixture": "bench/p2/scripts/testdata/acceptfix (stdlib only, offline buildable)",
	}
	res.Budgets["p95_le_budget"] = res.P95Ms <= *budgetMs
	res.Budgets["all_pass_verdict"] = res.Verdicts["pass"] == len(lat)
	res.Passed = res.Budgets["p95_le_budget"] && res.Budgets["all_pass_verdict"]
	if res.Verdicts["pass"] != len(lat) {
		res.Notes = append(res.Notes, "some iterations did not return a pass verdict; inspect checks output")
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
	fmt.Fprintf(os.Stderr, "acceptbench: "+format+"\n", args...)
	os.Exit(1)
}
