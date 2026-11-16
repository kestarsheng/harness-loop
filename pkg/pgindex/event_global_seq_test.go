package pgindex

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// applyEventGlobalSeqDDL 重放 014 的整个 DDL 文件。
// 迁移框架只会在 014 未记录时执行一次，所以「可重放」这件事必须由测试直接验证：
// 存量库补跑、回填被中断后重跑，走的都是这条路径。
func applyEventGlobalSeqDDL(t *testing.T, db *DB) {
	t.Helper()
	body, err := ddlFS.ReadFile("ddl/014_event_global_seq.sql")
	if err != nil {
		t.Fatalf("read 014 ddl: %v", err)
	}
	if _, err := db.Pool().Exec(context.Background(), string(body), pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("replay 014 ddl: %v", err)
	}
}

// seedLegacyEvent 写一条「014 之前的历史行」：不提供 global_seq，模拟升级前的存量数据。
func seedLegacyEvent(t *testing.T, db *DB, tenantID, eventID, taskID string, seq int64, ts time.Time) {
	t.Helper()
	_, err := db.Exec(context.Background(), `
		INSERT INTO hlp_event.events
			(event_id, task_id, tenant_id, project_id, seq, event_type, schema_version, severity, ts, payload)
		VALUES ($1, $2, $3, 'proj-cursor', $4, 'stage.transition', '1.0.0', 'info', $5, '{}')`,
		eventID, taskID, tenantID, seq, ts)
	if err != nil {
		t.Fatalf("seed legacy event %s: %v", eventID, err)
	}
}

func cleanupTenant(t *testing.T, db *DB, tenantID string) {
	t.Helper()
	if _, err := db.Exec(context.Background(), `DELETE FROM hlp_event.events WHERE tenant_id = $1`, tenantID); err != nil {
		t.Logf("cleanup tenant %s: %v", tenantID, err)
	}
}

func globalSeqOf(t *testing.T, db *DB, eventID string) int64 {
	t.Helper()
	var v *int64
	if err := db.QueryRow(context.Background(),
		`SELECT global_seq FROM hlp_event.events WHERE event_id = $1`, eventID).Scan(&v); err != nil {
		t.Fatalf("read global_seq of %s: %v", eventID, err)
	}
	if v == nil {
		t.Fatalf("event %s still has NULL global_seq", eventID)
	}
	return *v
}

// TestEventGlobalSeqBackfill 覆盖 T-P2-17 的迁移要求：
// 历史行按 (ts, event_id) 在租户内连续编号、无重复；重放不改变既有值；
// 已有部分 global_seq 的租户从 max+1 继续。
func TestEventGlobalSeqBackfill(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tenant := fmt.Sprintf("tenant-cursor-backfill-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupTenant(t, db, tenant) })

	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)
	// e2 与 e3 同一时间戳，靠 event_id 决定先后：e2("...b") 排在 e3("...c") 之前。
	seedLegacyEvent(t, db, tenant, tenant+"-a", "task-1", 1, base)
	seedLegacyEvent(t, db, tenant, tenant+"-c", "task-2", 1, base.Add(time.Second))
	seedLegacyEvent(t, db, tenant, tenant+"-b", "task-2", 2, base.Add(time.Second))

	applyEventGlobalSeqDDL(t, db)

	want := map[string]int64{
		tenant + "-a": 1,
		tenant + "-b": 2,
		tenant + "-c": 3,
	}
	for id, wantSeq := range want {
		if got := globalSeqOf(t, db, id); got != wantSeq {
			t.Fatalf("global_seq(%s) = %d, want %d", id, got, wantSeq)
		}
	}

	// 重放：回填过的行不再变化，约束也不会重复创建。
	applyEventGlobalSeqDDL(t, db)
	for id, wantSeq := range want {
		if got := globalSeqOf(t, db, id); got != wantSeq {
			t.Fatalf("replay changed global_seq(%s) = %d, want %d", id, got, wantSeq)
		}
	}

	// 增量路径：租户已有 max(global_seq)=100 时，新到的历史行必须从 101 继续。
	seedLegacyEvent(t, db, tenant, tenant+"-old", "task-3", 1, base.Add(2*time.Second))
	if _, err := db.Exec(ctx,
		`UPDATE hlp_event.events SET global_seq = 100 WHERE event_id = $1`, tenant+"-a"); err != nil {
		t.Fatalf("pin max global_seq: %v", err)
	}
	applyEventGlobalSeqDDL(t, db)
	if got := globalSeqOf(t, db, tenant+"-old"); got != 101 {
		t.Fatalf("incremental backfill = %d, want 101", got)
	}

	var total, distinct int
	if err := db.QueryRow(ctx, `
		SELECT count(*), count(DISTINCT global_seq)
		FROM hlp_event.events WHERE tenant_id = $1`, tenant).Scan(&total, &distinct); err != nil {
		t.Fatalf("count tenant events: %v", err)
	}
	if total != distinct {
		t.Fatalf("tenant %s has duplicate global_seq: %d rows, %d distinct", tenant, total, distinct)
	}

	// 唯一约束必须真实存在：并发写入靠它兜住读-改-写的漏网。
	var hasUnique bool
	if err := db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_constraint
			WHERE conname = 'uq_events_tenant_global_seq'
			  AND conrelid = 'hlp_event.events'::regclass
		)`).Scan(&hasUnique); err != nil {
		t.Fatalf("query unique constraint: %v", err)
	}
	if !hasUnique {
		t.Fatal("hlp_event.events must have unique constraint (tenant_id, global_seq)")
	}
}
