// Command probe 是 T-P2-T2 混沌演练的客户端探针。
//
// 设计取舍：docker 层面的注入（pause/stop/kill/--tmpfs）由 bench/chaos/inject.sh
// 负责，而"平台侧到底看到了什么"必须走真实客户端（pgx / mongo-driver /
// go-redis），否则演练只证明容器停了，没证明代码没有静默放行。因此所有探测都
// 走 pkg/pgindex、pkg/mongodb、pkg/redis 这三条真实链路。
//
// 输出统一为单行 JSON，便于 inject.sh 用 grep 断言；预期失败用非 0 退出码。
package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-platform/pkg/mongodb"
	"agent-platform/pkg/pgindex"
	"agent-platform/pkg/redis"
)

const (
	chaosTaskID = "chaos-task"
	chaosTenant = "chaos-tenant"
	chaosProj   = "chaos-project"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func pgDSN() string {
	return env("CHAOS_PG_DSN", "postgres://agent:agent_dev_password@127.0.0.1:55440/agent_platform?sslmode=disable")
}

func mongoURI() string {
	return env("CHAOS_MONGO_URI", "mongodb://127.0.0.1:27018")
}

func redisURL() string {
	return env("CHAOS_REDIS_URL", "redis://127.0.0.1:56379/0")
}

func emit(payload map[string]any) {
	payload["at"] = time.Now().UTC().Format(time.RFC3339Nano)
	raw, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe: marshal result: %v\n", err)
		os.Exit(2)
	}
	fmt.Println(string(raw))
}

func fail(op string, err error) {
	emit(map[string]any{
		"op":       op,
		"result":   "error",
		"error":    err.Error(),
		"surfaced": true,
	})
	os.Exit(1)
}

func main() {
	op := flag.String("op", "", "operation: pg-migrate|pg-digest|pg-tx|pg-probe|mongo-init|mongo-digest|mongo-probe|redis-init|redis-digest|redis-probe|disk-full")
	rows := flag.Int("rows", 8, "number of fixture rows")
	hold := flag.Duration("hold", 30*time.Second, "how long pg-tx keeps the transaction open")
	dir := flag.String("dir", "", "directory for disk-full")
	sizeMB := flag.Int("size-mb", 8, "payload size for disk-full")
	flag.Parse()

	opName := strings.TrimSpace(*op)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	switch opName {
	case "pg-migrate":
		runPGMigrate(ctx, *rows)
	case "pg-digest":
		runPGDigest(ctx)
	case "pg-tx":
		runPGTx(ctx, *hold)
	case "pg-probe":
		runPGProbe(ctx)
	case "mongo-init":
		runMongoInit(ctx)
	case "mongo-digest":
		runMongoDigest(ctx)
	case "mongo-probe":
		runMongoProbe(ctx)
	case "redis-init":
		runRedisInit(ctx)
	case "redis-digest":
		runRedisDigest(ctx)
	case "redis-probe":
		runRedisProbe(ctx)
	case "disk-full":
		runDiskFull(*dir, *sizeMB)
	default:
		fmt.Fprintf(os.Stderr, "probe: unknown -op %q\n", opName)
		os.Exit(2)
	}
}

func openPG(ctx context.Context) (*pgindex.DB, error) {
	db, err := pgindex.Open(ctx, pgindex.DefaultConfig(pgDSN()))
	if err != nil {
		return nil, err
	}
	return db, nil
}

func runPGMigrate(ctx context.Context, rows int) {
	db, err := openPG(ctx)
	if err != nil {
		fail("pg-migrate", err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		fail("pg-migrate", err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM hlp_task.subtasks WHERE parent_task_id = $1`, chaosTaskID); err != nil {
		fail("pg-migrate", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO hlp_task.task_runs
			(task_id, tenant_id, project_id, workspace_root, goal, goal_hash, current_stage, status, policy_snapshot)
		VALUES ($1,$2,$3,'/workspace','chaos goal','sha256:chaos','c','running','{"version":1}')
		ON CONFLICT (task_id) DO NOTHING`,
		chaosTaskID, chaosTenant, chaosProj); err != nil {
		fail("pg-migrate", err)
	}
	for i := 0; i < rows; i++ {
		id := fmt.Sprintf("chaos-st-%02d", i)
		if _, err := db.Exec(ctx, `
			INSERT INTO hlp_task.subtasks
				(subtask_id, parent_task_id, tenant_id, project_id, dag_id, dag_node_id, depth,
				 index_in_parent, parallel_set_id, role, status, failure_strategy, attempt_no, alloc_tokens)
			VALUES ($1,$2,$3,$4,'dag-chaos',$5,1,$6,'p1','coder','pending','fail-fast',1,128)
			ON CONFLICT (subtask_id) DO NOTHING`,
			id, chaosTaskID, chaosTenant, chaosProj, id, i); err != nil {
			fail("pg-migrate", err)
		}
	}
	runPGDigest(ctx)
}

func runPGDigest(ctx context.Context) {
	db, err := openPG(ctx)
	if err != nil {
		fail("pg-digest", err)
	}
	defer db.Close()
	var count int
	var digest string
	if err := db.QueryRow(ctx, `
		SELECT count(*), coalesce(md5(string_agg(row_repr, '|' ORDER BY row_repr)), 'empty')
		FROM (
			SELECT subtask_id || ':' || status || ':' || attempt_no || ':' || alloc_tokens AS row_repr
			FROM hlp_task.subtasks
			WHERE parent_task_id = $1
		) s`, chaosTaskID).Scan(&count, &digest); err != nil {
		fail("pg-digest", err)
	}
	emit(map[string]any{"op": "pg-digest", "result": "ok", "rows": count, "digest": digest})
}

// runPGTx opens a transaction, inserts one row and holds it open. The harness
// kills the container (or this process) mid-flight: after recovery the row must
// not exist, which is the "kill -9 不产生半写" evidence.
func runPGTx(ctx context.Context, hold time.Duration) {
	db, err := openPG(ctx)
	if err != nil {
		fail("pg-tx", err)
	}
	defer db.Close()
	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		fail("pg-tx", err)
	}
	rowID := fmt.Sprintf("chaos-tx-%d", time.Now().UnixNano())
	if _, err := tx.Exec(ctx,
		`INSERT INTO hlp_task.subtasks
			(subtask_id, parent_task_id, tenant_id, project_id, dag_id, dag_node_id, depth,
			 index_in_parent, parallel_set_id, role, status, failure_strategy, attempt_no, alloc_tokens)
		 VALUES ($1,$2,$3,$4,'dag-tx','tx-node',1,0,'p1','coder','running','fail-fast',1,64)`,
		rowID, chaosTaskID, chaosTenant, chaosProj); err != nil {
		fail("pg-tx", err)
	}
	emit(map[string]any{"op": "pg-tx", "result": "tx-open", "row": rowID})
	time.Sleep(hold)
	if err := tx.Commit(ctx); err != nil {
		fail("pg-tx", err)
	}
	emit(map[string]any{"op": "pg-tx", "result": "committed", "row": rowID})
}

// runPGProbe expects the dependency to be down: it succeeds (exit 0) only when
// the failure is surfaced as an error instead of a silent success.
func runPGProbe(ctx context.Context) {
	db, err := openPG(ctx)
	if err != nil {
		emit(map[string]any{"op": "pg-probe", "result": "degraded", "stage": "open", "error": err.Error()})
		return
	}
	defer db.Close()
	if _, err := db.Exec(ctx, `SELECT 1`); err != nil {
		emit(map[string]any{"op": "pg-probe", "result": "degraded", "stage": "exec", "error": err.Error()})
		return
	}
	emit(map[string]any{"op": "pg-probe", "result": "ok"})
}

func openMongo(ctx context.Context) (*mongodb.Client, error) {
	return mongodb.NewClient(ctx, mongodb.Config{URI: mongoURI(), Database: "chaos"})
}

func runMongoInit(ctx context.Context) {
	client, err := openMongo(ctx)
	if err != nil {
		fail("mongo-init", err)
	}
	defer client.Close(ctx)
	for i := 0; i < 5; i++ {
		doc := &mongodb.Document{
			ID:        fmt.Sprintf("chaos-doc-%02d", i),
			Title:     "chaos checkpoint",
			Content:   fmt.Sprintf("payload-%d", i),
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		}
		if err := client.InsertDocument(ctx, doc); err != nil && !strings.Contains(err.Error(), "duplicate key") {
			fail("mongo-init", err)
		}
	}
	runMongoDigest(ctx)
}

func runMongoDigest(ctx context.Context) {
	client, err := openMongo(ctx)
	if err != nil {
		fail("mongo-digest", err)
	}
	defer client.Close(ctx)
	count, err := client.CountDocuments(ctx)
	if err != nil {
		fail("mongo-digest", err)
	}
	docs, err := client.ListDocuments(ctx, 100, 0)
	if err != nil {
		fail("mongo-digest", err)
	}
	joined := make([]string, 0, len(docs))
	for _, doc := range docs {
		joined = append(joined, doc.ID+":"+doc.Content)
	}
	sum := md5.Sum([]byte(strings.Join(joined, "|")))
	emit(map[string]any{
		"op":     "mongo-digest",
		"result": "ok",
		"rows":   count,
		"digest": hex.EncodeToString(sum[:]),
	})
}

func runMongoProbe(ctx context.Context) {
	client, err := openMongo(ctx)
	if err != nil {
		emit(map[string]any{"op": "mongo-probe", "result": "degraded", "stage": "open", "error": err.Error()})
		return
	}
	defer client.Close(ctx)
	if _, err := client.CountDocuments(ctx); err != nil {
		emit(map[string]any{"op": "mongo-probe", "result": "degraded", "stage": "count", "error": err.Error()})
		return
	}
	emit(map[string]any{"op": "mongo-probe", "result": "ok"})
}

func openRedis() (*redis.Client, error) {
	return redis.NewClient(redis.Config{URL: redisURL()})
}

func runRedisInit(ctx context.Context) {
	client, err := openRedis()
	if err != nil {
		fail("redis-init", err)
	}
	defer client.Close()
	for i := 0; i < 5; i++ {
		if err := client.Set(ctx, fmt.Sprintf("chaos:key:%02d", i), fmt.Sprintf("value-%d", i), 0); err != nil {
			fail("redis-init", err)
		}
	}
	runRedisDigest(ctx)
}

func runRedisDigest(ctx context.Context) {
	client, err := openRedis()
	if err != nil {
		fail("redis-digest", err)
	}
	defer client.Close()
	var parts []string
	for i := 0; i < 5; i++ {
		value, err := client.Get(ctx, fmt.Sprintf("chaos:key:%02d", i))
		if err != nil {
			fail("redis-digest", err)
		}
		parts = append(parts, value)
	}
	sum := md5.Sum([]byte(strings.Join(parts, "|")))
	emit(map[string]any{
		"op":     "redis-digest",
		"result": "ok",
		"rows":   len(parts),
		"digest": hex.EncodeToString(sum[:]),
	})
}

func runRedisProbe(ctx context.Context) {
	client, err := openRedis()
	if err != nil {
		emit(map[string]any{"op": "redis-probe", "result": "degraded", "stage": "open", "error": err.Error()})
		return
	}
	defer client.Close()
	if err := client.Set(ctx, "chaos:probe", "1", 0); err != nil {
		emit(map[string]any{"op": "redis-probe", "result": "degraded", "stage": "set", "error": err.Error()})
		return
	}
	emit(map[string]any{"op": "redis-probe", "result": "ok"})
}

// runDiskFull writes a payload larger than the tmpfs quota. The write path is
// "temp file + rename", so an ENOSPC must leave neither a half-written target
// nor a stray temp file.
func runDiskFull(dir string, sizeMB int) {
	if dir == "" {
		fmt.Fprintln(os.Stderr, "probe: -dir is required for disk-full")
		os.Exit(2)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail("disk-full", err)
	}
	target := filepath.Join(dir, "memory.md")
	temp := target + ".tmp"
	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	var writeErr error
	f, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		writeErr = err
	} else {
		for i := 0; i < sizeMB; i++ {
			if _, err := f.Write(payload); err != nil {
				writeErr = err
				break
			}
		}
		closeErr := f.Close()
		if writeErr == nil {
			writeErr = closeErr
		}
	}
	if writeErr == nil {
		writeErr = os.Rename(temp, target)
	}
	if writeErr != nil {
		// 明确失败：清掉临时文件，且绝不留下半写目标文件。
		_ = os.Remove(temp)
		entries, _ := os.ReadDir(dir)
		var leftovers []string
		for _, entry := range entries {
			leftovers = append(leftovers, entry.Name())
		}
		if _, statErr := os.Stat(target); statErr == nil {
			emit(map[string]any{"op": "disk-full", "result": "corrupted", "error": writeErr.Error(), "leftovers": leftovers})
			os.Exit(1)
		}
		emit(map[string]any{
			"op":         "disk-full",
			"result":     "surfaced",
			"error":      writeErr.Error(),
			"no_corrupt": true,
			"leftovers":  leftovers,
		})
		return
	}
	// 写入成功说明配额没打满，用例无效，必须显式失败而不是"通过"。
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		fail("disk-full", err)
	}
	emit(map[string]any{"op": "disk-full", "result": "not-full", "size_mb": sizeMB})
	os.Exit(1)
}
