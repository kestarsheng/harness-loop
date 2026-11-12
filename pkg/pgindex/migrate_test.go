package pgindex

import (
	"context"
	"net/url"
	"os"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestMigrateAll(t *testing.T) {
	dsn := os.Getenv("HARNESS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("HARNESS_TEST_PG_DSN not set")
	}
	dbName := os.Getenv("HARNESS_TEST_PG_EMPTY_DB_NAME")
	if dbName == "" {
		dbName = "agent_platform_migtest"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, dsnWithDatabase(t, dsn, "postgres"))
	if err != nil {
		t.Skipf("cannot connect admin database: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })

	dropDatabase(t, admin, dbName)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+quoteIdent(dbName)); err != nil {
		t.Fatalf("create empty database %s: %v", dbName, err)
	}
	t.Cleanup(func() { dropDatabase(t, admin, dbName) })

	emptyDSN := dsnWithDatabase(t, dsn, dbName)
	db, err := Open(ctx, DefaultConfig(emptyDSN))
	if err != nil {
		t.Fatalf("open empty database: %v", err)
	}
	t.Cleanup(db.Close)

	var inherited int
	if err := db.QueryRow(ctx, `
		SELECT count(*) FROM pg_namespace
		WHERE nspname IN ('hlp_mem', 'hlp_code', 'hlp_task', 'hlp_event')`).Scan(&inherited); err != nil {
		t.Fatalf("inspect empty database: %v", err)
	}
	if inherited != 0 {
		t.Fatalf("database %s is not empty: %d harness schemas already exist", dbName, inherited)
	}
	var vectorInstalled bool
	if err := db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector')`).Scan(&vectorInstalled); err != nil {
		t.Fatalf("check vector extension: %v", err)
	}
	if vectorInstalled {
		t.Fatalf("database %s must start without the vector extension", dbName)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate empty database: %v", err)
	}
	first, err := migrationRecords(ctx, db)
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	versions := make([]string, len(first))
	for i, r := range first {
		versions[i] = r.Version
	}
	assertContinuousMigrations(t, versions)

	var vectorCreated bool
	if err := db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector')`).Scan(&vectorCreated); err != nil {
		t.Fatalf("check vector extension after migrate: %v", err)
	}
	if !vectorCreated {
		t.Fatal("migrator must create the vector extension before applying DDL")
	}
	assertModelSelectionComboIndex(t, db)

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("replay empty database: %v", err)
	}
	second, err := migrationRecords(ctx, db)
	if err != nil {
		t.Fatalf("list migrations after replay: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("replay changed schema_migrations:\nfirst:  %+v\nsecond: %+v", first, second)
	}
	t.Logf("empty database %s: %d migrations applied once, replay produced no changes", dbName, len(first))
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	first, err := migrationRecords(ctx, db)
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("expected applied migrations")
	}
	versions := make([]string, len(first))
	for i, r := range first {
		versions[i] = r.Version
	}
	assertContinuousMigrations(t, versions)

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	second, err := migrationRecords(ctx, db)
	if err != nil {
		t.Fatalf("list migrations again: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("replay changed schema_migrations:\nfirst:  %+v\nsecond: %+v", first, second)
	}
	assertModelSelectionComboIndex(t, db)
}

func TestSchemaMatchesDesign(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	cases := []struct {
		table  string
		column string
	}{
		{"hlp_task.task_runs", "version"},
		{"hlp_task.task_runs", "policy_snapshot"},
		{"hlp_task.task_runs", "fsm_enabled"},
		{"hlp_task.stage_attempts", "entry_reason"},
		{"hlp_task.acceptance_records", "rollback_target"},
		{"hlp_event.events", "payload"},
		{"hlp_event.events", "subtask_id"},
		{"hlp_event.audit", "rule_id"},
		{"hlp_event.tool_idempotency", "idempotency_key"},
		{"hlp_task.model_selections", "attempt_no"},
		{"hlp_task.model_selections", "reason"},
		{"hlp_task.model_selections", "context_window"},
		{"hlp_task.model_selections", "threshold_tokens"},
		{"hlp_task.model_selections", "input_price"},
		{"hlp_task.model_selections", "output_price"},
		{"hlp_task.model_selections", "is_fallback"},
		{"hlp_task.model_selections", "approval_id"},
		{"hlp_task.model_selections", "operator"},
	}
	for _, c := range cases {
		var exists bool
		if err := db.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = split_part($1, '.', 1)
				  AND table_name = split_part($1, '.', 2)
				  AND column_name = $2
			)`, c.table, c.column).Scan(&exists); err != nil {
			t.Fatalf("query column %s.%s: %v", c.table, c.column, err)
		}
		if !exists {
			t.Fatalf("column %s.%s missing", c.table, c.column)
		}
	}

	var hasUniqueTaskSeq bool
	if err := db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_constraint c
			JOIN pg_class t ON t.oid = c.conrelid
			JOIN pg_namespace n ON n.oid = t.relnamespace
			WHERE n.nspname = 'hlp_event' AND t.relname = 'events' AND c.contype = 'u'
		)`).Scan(&hasUniqueTaskSeq); err != nil {
		t.Fatalf("query unique constraint: %v", err)
	}
	if !hasUniqueTaskSeq {
		t.Fatal("hlp_event.events must have a unique constraint (task_id, seq)")
	}

	var hasLegacyWindow bool
	if err := db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = 'hlp_task'
			  AND table_name = 'model_selections'
			  AND column_name = 'window'
		)`).Scan(&hasLegacyWindow); err != nil {
		t.Fatalf("query legacy window column: %v", err)
	}
	if hasLegacyWindow {
		t.Fatal("hlp_task.model_selections must not use reserved column name window")
	}

	rows, err := db.Query(ctx, `
		SELECT pg_get_constraintdef(c.oid)
		FROM pg_constraint c
		JOIN pg_class t ON t.oid = c.conrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE n.nspname = 'hlp_task' AND t.relname = 'model_selections' AND c.contype = 'c'`)
	if err != nil {
		t.Fatalf("query model_selections checks: %v", err)
	}
	defer rows.Close()
	var defs strings.Builder
	for rows.Next() {
		var def string
		if err := rows.Scan(&def); err != nil {
			t.Fatalf("scan check: %v", err)
		}
		defs.WriteString(def)
		defs.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate checks: %v", err)
	}
	if !strings.Contains(defs.String(), "'explicit'") {
		t.Fatalf("strategy check must allow explicit: %s", defs.String())
	}
	if !strings.Contains(defs.String(), "'fallback'") {
		t.Fatalf("resolved_from check must allow fallback: %s", defs.String())
	}

	assertModelSelectionComboIndex(t, db)

	applied, err := AppliedMigrations(ctx, db)
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	assertContinuousMigrations(t, applied)
}

func TestSyncMatrix(t *testing.T) {
	rules := SyncMatrix()
	if len(rules) != len(syncRules) {
		t.Fatalf("matrix size = %d, want %d", len(rules), len(syncRules))
	}
	flags := make(map[string]struct{}, len(rules))
	for _, rule := range rules {
		if rule.Flag == "" || rule.NewWrite == "" || rule.NewRead == "" ||
			rule.LegacyWrite == "" || rule.LegacyRead == "" || rule.Rollback == "" {
			t.Fatalf("incomplete sync rule: %+v", rule)
		}
		if _, dup := flags[rule.Flag]; dup {
			t.Fatalf("duplicate sync flag %q", rule.Flag)
		}
		flags[rule.Flag] = struct{}{}

		on, err := ResolveSyncRoute(rule.Scenario, true)
		if err != nil {
			t.Fatalf("resolve %s on: %v", rule.Scenario, err)
		}
		if on.WriteTarget != rule.NewWrite || on.ReadPath != rule.NewRead || on.Degraded {
			t.Fatalf("%s enabled route = %+v", rule.Scenario, on)
		}

		off, err := ResolveSyncRoute(rule.Scenario, false)
		if err != nil {
			t.Fatalf("resolve %s off: %v", rule.Scenario, err)
		}
		if off.WriteTarget != rule.LegacyWrite || off.ReadPath != rule.LegacyRead {
			t.Fatalf("%s disabled route = %+v", rule.Scenario, off)
		}
		if off.ReadPath == "" {
			t.Fatalf("%s disabled route has no usable read path", rule.Scenario)
		}
	}

	memory, err := ResolveSyncRoute(SyncMemory, false)
	if err != nil {
		t.Fatalf("resolve memory off: %v", err)
	}
	if memory.ReadPath != "markdown" || memory.WriteTarget != "markdown" || memory.Degraded {
		t.Fatalf("memory legacy path must stay usable: %+v", memory)
	}

	codeindex, err := ResolveSyncRoute(SyncCodeIndex, false)
	if err != nil {
		t.Fatalf("resolve codeindex off: %v", err)
	}
	if !codeindex.Degraded || codeindex.ReadPath == "" {
		t.Fatalf("codeindex disabled route must degrade explicitly: %+v", codeindex)
	}

	if _, err := ResolveSyncRoute("unknown", true); err == nil {
		t.Fatal("unknown scenario must fail")
	}
}

type migrationRecord struct {
	Version   string
	Checksum  string
	AppliedAt time.Time
}

func migrationRecords(ctx context.Context, db *DB) ([]migrationRecord, error) {
	rows, err := db.Query(ctx,
		`SELECT version, checksum, applied_at FROM hlp_event.schema_migrations ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []migrationRecord
	for rows.Next() {
		var r migrationRecord
		if err := rows.Scan(&r.Version, &r.Checksum, &r.AppliedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func expectedDDLVersions(t *testing.T) []string {
	t.Helper()
	entries, err := ddlFS.ReadDir("ddl")
	if err != nil {
		t.Fatalf("read ddl dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, strings.TrimSuffix(e.Name(), ".sql"))
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("no ddl files embedded")
	}
	return names
}

func assertContinuousMigrations(t *testing.T, got []string) {
	t.Helper()
	want := expectedDDLVersions(t)
	if !slices.Equal(got, want) {
		t.Fatalf("schema_migrations = %v, want %v", got, want)
	}
	for i, version := range got {
		prefix, _, ok := strings.Cut(version, "_")
		if !ok {
			t.Fatalf("migration %q has no numeric prefix", version)
		}
		n, err := strconv.Atoi(prefix)
		if err != nil || n != i+1 {
			t.Fatalf("migration sequence not continuous at %q (want %03d)", version, i+1)
		}
	}
}

func assertModelSelectionComboIndex(t *testing.T, db *DB) {
	t.Helper()
	var def string
	if err := db.QueryRow(context.Background(), `
		SELECT indexdef FROM pg_indexes
		WHERE schemaname = 'hlp_task'
		  AND tablename = 'model_selections'
		  AND indexname = 'idx_model_selection_combo'`).Scan(&def); err != nil {
		t.Fatalf("idx_model_selection_combo missing: %v", err)
	}
	if !strings.Contains(def, "(stage, model_id)") {
		t.Fatalf("idx_model_selection_combo columns mismatch: %s", def)
	}
}

func dsnWithDatabase(t *testing.T, raw, database string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse HARNESS_TEST_PG_DSN: %v", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		t.Fatalf("HARNESS_TEST_PG_DSN must be a postgres:// URL, got %q", raw)
	}
	u.Path = "/" + database
	return u.String()
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func dropDatabase(t *testing.T, admin *pgx.Conn, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS `+quoteIdent(name)+` WITH (FORCE)`); err != nil {
		t.Logf("drop database %s: %v", name, err)
	}
}
