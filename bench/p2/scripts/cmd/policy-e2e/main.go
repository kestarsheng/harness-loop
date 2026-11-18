// T-P2-25 活栈探针：用真实任务触发 read_file / run_command，验证
// policy 决策 / 审批 / 审计三者一一对应。
//
// 与单测的区别：这里打的是已经跑起来的 50013(harnessloop-service) + 50005(mcp-service)
// 栈，agent 是真 agent-service + 真 LLM（不是脚本）。所以本程序不做「必须通过」的
// 断言，只如实采集：任务终态、事件流、审计行，以及「发出过的工具调用」这一列到底
// 有没有出现。工具调用没触发时，输出 must_trigger=false 并给出原因，不能拿 audit=0
// 冒充通过。
//
// 用法：
//
//	go run ./bench/p2/scripts/cmd/policy-e2e -goal "读取 README.md 并执行 go version"
//
// 前置：50013 与 50005 已在运行；PG 可连（用于回读 hlp_event.audit）。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	harnesspb "agent-platform/pkg/pb/harnessloop"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:50013", "harnessloop-service gRPC 地址")
	tenant := flag.String("tenant", "tenant-policy", "租户")
	project := flag.String("project", "p2-audit", "项目")
	workspace := flag.String("workspace", "bench/p2/policy-audit-workspace", "任务工作区（绝对路径或相对仓库根）")
	goal := flag.String("goal", "读取 README.md 的内容，然后执行 go version 并汇报版本号。", "任务目标")
	dsn := flag.String("dsn", os.Getenv("HARNESS_TEST_PG_DSN"), "PG DSN（用于回读审计）")
	timeout := flag.Duration("timeout", 4*time.Minute, "等待任务终态的最长时间")
	flag.Parse()

	ws, err := absPath(*workspace)
	if err != nil {
		fail("resolve workspace: %v", err)
	}

	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fail("dial %s: %v", *addr, err)
	}
	defer conn.Close()
	client := harnesspb.NewHarnessLoopServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	created, err := client.CreateTask(ctx, &harnesspb.CreateTaskRequest{
		TenantId:      *tenant,
		ProjectId:     *project,
		WorkspaceRoot: ws,
		Goal:          *goal,
	})
	if err != nil {
		fail("CreateTask: %v", err)
	}
	taskID := created.GetTask().GetTaskId()
	fmt.Printf("task_id=%s workspace=%s\n", taskID, ws)

	var status, stage, reason string
	deadline := time.Now().Add(*timeout - 5*time.Second)
	for time.Now().Before(deadline) {
		view, getErr := client.GetTask(ctx, &harnesspb.GetTaskRequest{TenantId: *tenant, TaskId: taskID})
		if getErr != nil {
			fail("GetTask: %v", getErr)
		}
		status = view.GetStatus()
		stage = view.GetCurrentStage()
		reason = view.GetTerminalReason()
		if status == "completed" || status == "failed" || status == "aborted" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Printf("status=%s stage=%s reason=%s\n", status, stage, reason)

	events, err := client.ListEvents(ctx, &harnesspb.ListEventsRequest{TenantId: *tenant, TaskId: taskID, Limit: 500})
	if err == nil {
		byType := map[string]int{}
		for _, e := range events.GetEvents() {
			byType[e.GetEventType()]++
			if e.GetEventType() == "tool.denied" || e.GetEventType() == "approval.requested" ||
				e.GetEventType() == "security.violation" || e.GetEventType() == "error" {
				fmt.Printf("event %-20s stage=%s payload=%s\n", e.GetEventType(), e.GetStage(), truncate(string(e.GetPayload()), 240))
			}
		}
		fmt.Printf("events: %s\n", mustJSON(byType))
	}

	if *dsn == "" {
		fmt.Println("audit: skipped (no dsn)")
		return
	}
	rows, err := readAudit(ctx, *dsn, *tenant, taskID)
	if err != nil {
		fail("read audit: %v", err)
	}
	fmt.Printf("audit_rows=%d\n", len(rows))
	for _, r := range rows {
		fmt.Printf("audit %-46s tool=%-12s stage=%s decision=%-16s rule=%s code=%s target=%q\n",
			r.AuditID, r.Tool, r.Stage, r.Decision, r.RuleID, r.Code, r.Target)
	}
	// 有工具调用 → 必须有审计；没有工具调用 → 如实说未触发。
	fmt.Printf("must_trigger=%v\n", len(rows) > 0)
}

type auditRow struct {
	AuditID  string
	Tool     string
	Target   string
	Decision string
	RuleID   string
	Stage    string
	Code     string
}

func readAudit(ctx context.Context, dsn, tenant, taskID string) ([]auditRow, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, `
		SELECT audit_id, action, COALESCE(target,''), COALESCE(decision,''),
		       COALESCE(rule_id,''), COALESCE(payload->>'stage',''), COALESCE(payload->>'code','')
		FROM hlp_event.audit
		WHERE tenant_id = $1 AND task_id = $2 AND kind = 'tool_decision'
		ORDER BY created_at, audit_id`, tenant, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.AuditID, &r.Tool, &r.Target, &r.Decision, &r.RuleID, &r.Stage, &r.Code); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func absPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	if os.IsPathSeparator(p[0]) || (len(p) > 1 && p[1] == ':') {
		return p, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%c%s", wd, os.PathSeparator, p), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "policy-e2e: "+format+"\n", args...)
	os.Exit(1)
}
