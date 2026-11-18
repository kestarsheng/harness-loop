package repository

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	eventpkg "agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/pgindex"
)

type EventFilter struct {
	TenantID  string
	TaskID    string
	Stage     string
	EventType string
	Severity  string
	// SinceSeq 是租户级游标（global_seq），不是任务内序号：List 返回
	// global_seq > SinceSeq 的事件，按 global_seq 升序。
	SinceSeq int64
	Limit    int
}

type EventRepo struct {
	db *pgindex.DB
}

func NewEventRepo(db *pgindex.DB) *EventRepo { return &EventRepo{db: db} }

// RepoStats 是事件写入侧的累计计数（进程内），用于把「撞键重试」「静默丢弃」
// 这类过去只能靠反推的现象变成可观测指标。
type RepoStats struct {
	// Appended 成功写入的事件行数。
	Appended int64
	// Deduped event_id 重复被跳过的行数（幂等重投的正常结果）。
	Deduped int64
	// SeqRetries 撞 (task_id, seq) 后重新分配 seq 并重试成功的次数。
	SeqRetries int64
	// SeqConflicts 重试耗尽仍然撞键、最终放弃的事件数（这些事件会返回错误，
	// 由发布器缓冲重投，不会静默消失）。
	SeqConflicts int64
}

var eventRepoStats struct {
	appended     atomic.Int64
	deduped      atomic.Int64
	seqRetries   atomic.Int64
	seqConflicts atomic.Int64
}

// Stats 返回写入侧累计计数快照。
func (r *EventRepo) Stats() RepoStats {
	return RepoStats{
		Appended:     eventRepoStats.appended.Load(),
		Deduped:      eventRepoStats.deduped.Load(),
		SeqRetries:   eventRepoStats.seqRetries.Load(),
		SeqConflicts: eventRepoStats.seqConflicts.Load(),
	}
}

// seqRetryAttempts 是撞键后的重试上限。正常路径（seq 由本仓库单一分配）永不
// 触发；只有滚动升级期间旧版本写者并发抢号才会走到这里。
const seqRetryAttempts = 3

// Append 是事件落库的唯一入口，也是「任务内 seq」的唯一分配者。
//
// # seq 契约（T-P2-32）
//
// seq 在写入事务内、租户 advisory lock 之下按 task 取 MAX(seq)+1，并由
// RETURNING 回填到入参 Event。调用方（event.Publisher / runtime 状态转移）
// 不得自行编号：此前发布器内存里从 1 开始编号、runtime 直写 DB 取 max+1，
// 两条写者各算各的，撞 (task_id, seq) 唯一键之后事件被吞掉——实测丢了
// stage b/c 的 tool.denied 与 task.completed。
//
// # 冲突处理
//
// 若仍有其它写者（滚动升级中的旧版本）先一步写入了同一个 seq，唯一键冲突会
// 在保存点内回滚后重新取 MAX(seq)+1 重试，最多 seqRetryAttempts 次。重试耗尽
// 才返回 ErrSeqConflict：宁可报错让上层缓冲重投，也不静默丢事件。
func (r *EventRepo) Append(ctx context.Context, tenantID, projectID string, events []*eventpkg.Event) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}
	for _, e := range events {
		if e == nil {
			continue
		}
		if e.TenantID == "" {
			e.TenantID = tenantID
		}
		if e.ProjectID == "" {
			e.ProjectID = projectID
		}
	}

	tx, err := r.db.Pool().Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("event_repo: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// global_seq 必须在租户维度严格单调。MAX(global_seq)+1 的读-改-写只有在
	// 「同租户同时只有一个写事务」时才成立，所以先取租户级 advisory 事务锁；
	// 锁随事务提交/回滚释放，不留在应用层（应用层缓存会跨进程漂移）。
	// 多租户批次按租户 ID 排序取锁，避免两个批次以相反顺序互锁。
	// 同一把锁同时保护 seq 的分配（任务级 MAX+1），所以两条写路径串行。
	for _, t := range batchTenants(events) {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, t); err != nil {
			return 0, fmt.Errorf("event_repo: lock tenant %s: %w", t, err)
		}
	}

	inserted := 0
	for _, e := range events {
		if e == nil {
			continue
		}
		if err := e.ValidateForPublish(); err != nil {
			return 0, err
		}
		ok, err := insertEventWithSeq(ctx, tx, e)
		if err != nil {
			return 0, err
		}
		if ok {
			inserted++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("event_repo: commit: %w", err)
	}
	return inserted, nil
}

// insertEventWithSeq 在保存点内写入一条事件，seq/global_seq 全部由库分配并回填。
// 返回 false 表示 event_id 已存在（幂等重投），不算错误。
func insertEventWithSeq(ctx context.Context, tx pgx.Tx, e *eventpkg.Event) (bool, error) {
	var (
		inserted  bool
		seq, gseq int64
	)
	err := withSeqRetry(ctx, tx, fmt.Sprintf("task_id=%s event_id=%s", e.TaskID, e.EventID), func(ctx context.Context, sp pgx.Tx) error {
		var err error
		inserted, seq, gseq, err = insertEventRow(ctx, sp, e)
		return err
	})
	if err != nil {
		return false, err
	}
	if !inserted {
		eventRepoStats.deduped.Add(1)
		return false, nil
	}
	e.Seq, e.GlobalSeq = seq, gseq
	eventRepoStats.appended.Add(1)
	return true, nil
}

// withSeqRetry 在保存点内执行一次「分配 seq 并写入」。撞上写入侧分配的唯一键
// ((task_id, seq) 或 (tenant_id, global_seq)) 时回滚保存点、重新取 MAX 后重试；
// 其它错误原样返回。正常路径（seq 由库单一分配 + 租户锁串行）永不重试。
func withSeqRetry(ctx context.Context, tx pgx.Tx, op string, insert func(ctx context.Context, sp pgx.Tx) error) error {
	var lastErr error
	for attempt := 1; attempt <= seqRetryAttempts; attempt++ {
		sp, err := tx.Begin(ctx) // SAVEPOINT：单条语句失败不拖死整个批次事务
		if err != nil {
			return fmt.Errorf("event_repo: savepoint: %w", err)
		}
		err = insert(ctx, sp)
		if err == nil {
			if cerr := sp.Commit(ctx); cerr != nil {
				return fmt.Errorf("event_repo: release savepoint: %w", cerr)
			}
			if attempt > 1 {
				eventRepoStats.seqRetries.Add(1)
				log.Printf("[event_repo] seq conflict resolved after %d attempts (%s)", attempt, op)
			}
			return nil
		}
		_ = sp.Rollback(ctx)
		if !isSeqUniqueViolation(err) {
			return err
		}
		lastErr = err
		log.Printf("[event_repo] seq conflict (attempt %d/%d, %s): %v; re-allocating", attempt, seqRetryAttempts, op, err)
	}
	eventRepoStats.seqConflicts.Add(1)
	log.Printf("[event_repo] seq conflict unresolved after %d attempts (%s): %v", seqRetryAttempts, op, lastErr)
	return fmt.Errorf("%w: %s: %v", eventpkg.ErrSeqConflict, op, lastErr)
}

// insertEventRow 执行一次插入：seq 与 global_seq 都在语句内分配（读到的是本
// 事务可见的最新 MAX），因此同批事件逐条 +1，不会因为 ON CONFLICT 跳行而留空洞。
func insertEventRow(ctx context.Context, q pgx.Tx, e *eventpkg.Event) (inserted bool, seq, globalSeq int64, err error) {
	row := q.QueryRow(ctx, `
		INSERT INTO hlp_event.events
			(event_id, task_id, tenant_id, project_id, seq, event_type, schema_version, stage, attempt,
			 severity, trace_id, span_id, ts, payload, payload_ref, agent_id,
			 parent_task_id, subtask_id, handoff_id, dag_id, dag_node_id, global_seq)
		SELECT $1,$2,$3,$4,
		       COALESCE((SELECT MAX(s.seq) FROM hlp_event.events s WHERE s.task_id = $2 AND s.tenant_id = $3), 0) + 1,
		       $5,$6,NULLIF($7,''),NULLIF($8,0),$9,NULLIF($10,''),NULLIF($11,''),$12,$13,NULLIF($14,''),NULLIF($20,''),
		       NULLIF($15,''),NULLIF($16,''),NULLIF($17,''),NULLIF($18,''),NULLIF($19,''),
		       COALESCE((SELECT MAX(g.global_seq) FROM hlp_event.events g WHERE g.tenant_id = $3), 0) + 1
		ON CONFLICT (event_id) DO NOTHING
		RETURNING seq, global_seq`,
		e.EventID, e.TaskID, e.TenantID, e.ProjectID, string(e.EventType), e.SchemaVersion,
		e.Stage, e.Attempt, string(e.Severity), e.TraceID, e.SpanID, e.Timestamp, e.Payload, e.PayloadRef,
		e.ParentTaskID, e.SubtaskID, e.HandoffID, e.DagID, e.DagNodeID, e.AgentID)
	if err := row.Scan(&seq, &globalSeq); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, 0, 0, nil
		}
		return false, 0, 0, err
	}
	return true, seq, globalSeq, nil
}

// isSeqUniqueViolation 判定是否撞上 (task_id, seq) / (tenant_id, global_seq)
// 这两个由写入侧分配的唯一键（event_id 主键冲突已被 ON CONFLICT 吃掉）。
func isSeqUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return false
	}
	switch pgErr.ConstraintName {
	case "events_task_id_seq_key", "uq_events_tenant_global_seq":
		return true
	}
	return false
}

// batchTenants 返回去重并按字典序排列的租户集合（取 advisory 锁的顺序）。
func batchTenants(events []*eventpkg.Event) []string {
	seen := make(map[string]struct{}, len(events))
	out := make([]string, 0, len(events))
	for _, e := range events {
		if e == nil || e.TenantID == "" {
			continue
		}
		if _, ok := seen[e.TenantID]; ok {
			continue
		}
		seen[e.TenantID] = struct{}{}
		out = append(out, e.TenantID)
	}
	sort.Strings(out)
	return out
}

const eventColumns = `event_id, event_type, schema_version, task_id, COALESCE(trace_id,''), COALESCE(span_id,''),
	COALESCE(stage,''), COALESCE(attempt,0), seq, ts, severity, tenant_id, project_id, COALESCE(agent_id,''),
	payload, COALESCE(payload_ref,''), COALESCE(parent_task_id,''), COALESCE(subtask_id,''), COALESCE(handoff_id,''),
	COALESCE(dag_id,''), COALESCE(dag_node_id,''), COALESCE(global_seq,0)`

func scanEvent(s rowScanner) (*eventpkg.Event, error) {
	var (
		e        eventpkg.Event
		typ, sev string
	)
	if err := s.Scan(&e.EventID, &typ, &e.SchemaVersion, &e.TaskID, &e.TraceID, &e.SpanID,
		&e.Stage, &e.Attempt, &e.Seq, &e.Timestamp, &sev, &e.TenantID, &e.ProjectID, &e.AgentID,
		&e.Payload, &e.PayloadRef, &e.ParentTaskID, &e.SubtaskID, &e.HandoffID, &e.DagID, &e.DagNodeID,
		&e.GlobalSeq); err != nil {
		return nil, err
	}
	e.EventType = eventpkg.EventType(typ)
	e.Severity = eventpkg.Severity(sev)
	return &e, nil
}

func (r *EventRepo) List(ctx context.Context, f EventFilter) ([]*eventpkg.Event, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.db.Query(ctx, `SELECT `+eventColumns+`
		FROM hlp_event.events
		WHERE tenant_id = $1
		  AND ($2 = '' OR task_id = $2)
		  AND ($3 = '' OR stage = $3)
		  AND ($4 = '' OR event_type = $4)
		  AND ($5 = '' OR severity = $5)
		  AND global_seq > $6
		ORDER BY global_seq
		LIMIT $7`,
		f.TenantID, f.TaskID, f.Stage, f.EventType, f.Severity, f.SinceSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("event_repo: list: %w", err)
	}
	defer rows.Close()

	var out []*eventpkg.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("event_repo: scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// NextSeq 返回某任务的「任务内」最大 seq，仅用于 Checkpoint 的
// event_seq_watermark 漂移判定（该水位与 seq 同口径，都是 per-task）。
// 它是水位，不是游标：跨任务增量拉取一律用 global_seq，见 WatchFrom。
func (r *EventRepo) NextSeq(ctx context.Context, tenantID, taskID string) (int64, error) {
	var seq int64
	err := r.db.QueryRow(ctx, `SELECT COALESCE(MAX(seq), 0) FROM hlp_event.events WHERE tenant_id = $1 AND task_id = $2`, tenantID, taskID).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("event_repo: next seq: %w", err)
	}
	return seq, nil
}

func (r *EventRepo) WatchFrom(ctx context.Context, tenantID string, taskIDs []string, sinceSeq int64, limit int) ([]*eventpkg.Event, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := r.db.Query(ctx, `SELECT `+eventColumns+`
		FROM hlp_event.events
		WHERE tenant_id = $1
		  AND global_seq > $2
		  AND (COALESCE(cardinality($3::text[]), 0) = 0 OR task_id = ANY($3))
		ORDER BY global_seq
		LIMIT $4`, tenantID, sinceSeq, taskIDs, limit)
	if err != nil {
		return nil, sinceSeq, fmt.Errorf("event_repo: watch: %w", err)
	}
	defer rows.Close()

	var (
		out  []*eventpkg.Event
		last = sinceSeq
	)
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, sinceSeq, fmt.Errorf("event_repo: scan: %w", err)
		}
		out = append(out, e)
		if e.GlobalSeq > last {
			last = e.GlobalSeq
		}
	}
	return out, last, rows.Err()
}

func (r *EventRepo) Cleanup(ctx context.Context, olderThan time.Time, limit int) (int64, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	tag, err := r.db.Exec(ctx, `
		DELETE FROM hlp_event.events
		WHERE ctid IN (
			SELECT ctid FROM hlp_event.events WHERE ts < $1 ORDER BY ts LIMIT $2
		)`, olderThan, limit)
	if err != nil {
		return 0, fmt.Errorf("event_repo: cleanup: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (r *EventRepo) GetByID(ctx context.Context, tenantID, eventID string) (*eventpkg.Event, error) {
	row := r.db.QueryRow(ctx, `SELECT `+eventColumns+` FROM hlp_event.events WHERE tenant_id = $1 AND event_id = $2`, tenantID, eventID)
	e, err := scanEvent(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("event_repo: get %s: %w", eventID, err)
	}
	return e, nil
}
