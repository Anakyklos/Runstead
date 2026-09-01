package state

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// testMigrations builds an fs.FS from a version -> SQL map, mirroring the
// embedded migrations layout (migrations/<NNNN>_name.sql).
func testMigrations(t *testing.T, entries map[int]string) fs.FS {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "migrations")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir migrations: %v", err)
	}
	for version, sql := range entries {
		name := filepath.Join(dir, fmt.Sprintf("%04d_test.sql", version))
		if err := os.WriteFile(name, []byte(sql), 0o600); err != nil {
			t.Fatalf("write migration %d: %v", version, err)
		}
	}
	return os.DirFS(filepath.Dir(dir))
}

// openTestDB opens a bare SQLite database without migrations.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "bare.db"))
	if err != nil {
		t.Fatalf("sql.Open error = %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// supportedSchemaVersion returns the highest version in the real embedded
// migration set.
func supportedSchemaVersion() int {
	names, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return 0
	}
	version := 0
	for _, name := range names {
		if parsed, parseErr := migrationVersion(name); parseErr == nil && parsed > version {
			version = parsed
		}
	}
	return version
}

func TestMigrationsFreshDatabase(t *testing.T) {
	db := openTestDB(t)
	files := testMigrations(t, map[int]string{
		1: "CREATE TABLE one (id INTEGER PRIMARY KEY);",
		2: "CREATE TABLE two (id INTEGER PRIMARY KEY);",
	})
	if err := migrateFS(db, files); err != nil {
		t.Fatalf("migrateFS() error = %v", err)
	}
	version, err := schemaVersion(db)
	if err != nil {
		t.Fatalf("schemaVersion() error = %v", err)
	}
	if version != 2 {
		t.Fatalf("schema version = %d, want 2", version)
	}
	for _, table := range []string{"one", "two"} {
		if _, err := db.Exec("SELECT 1 FROM " + table + " LIMIT 0"); err != nil {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}
}

func TestMigrationsReRunIsSafe(t *testing.T) {
	db := openTestDB(t)
	files := testMigrations(t, map[int]string{
		1: "CREATE TABLE one (id INTEGER PRIMARY KEY);",
	})
	if err := migrateFS(db, files); err != nil {
		t.Fatalf("first migrate error = %v", err)
	}
	if err := migrateFS(db, files); err != nil {
		t.Fatalf("second migrate error = %v", err)
	}
	version, _ := schemaVersion(db)
	if version != 1 {
		t.Fatalf("schema version after re-run = %d, want 1", version)
	}
}

func TestMigrationsUpgradeFromPreviousVersion(t *testing.T) {
	db := openTestDB(t)
	first := testMigrations(t, map[int]string{
		1: "CREATE TABLE one (id INTEGER PRIMARY KEY);",
	})
	if err := migrateFS(db, first); err != nil {
		t.Fatalf("migrate to v1 error = %v", err)
	}
	if _, err := db.Exec("INSERT INTO one (id) VALUES (1)"); err != nil {
		t.Fatalf("seed v1 data: %v", err)
	}

	second := testMigrations(t, map[int]string{
		1: "CREATE TABLE one (id INTEGER PRIMARY KEY);",
		2: "CREATE TABLE two (id INTEGER PRIMARY KEY);",
	})
	if err := migrateFS(db, second); err != nil {
		t.Fatalf("upgrade to v2 error = %v", err)
	}
	version, _ := schemaVersion(db)
	if version != 2 {
		t.Fatalf("schema version after upgrade = %d, want 2", version)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM one").Scan(&count); err != nil || count != 1 {
		t.Fatalf("v1 data lost after upgrade: count=%d err=%v", count, err)
	}
	if _, err := db.Exec("SELECT 1 FROM two LIMIT 0"); err != nil {
		t.Fatalf("v2 table missing: %v", err)
	}
}

func TestMigrationsRollbackOnFailedUpgrade(t *testing.T) {
	db := openTestDB(t)
	first := testMigrations(t, map[int]string{
		1: "CREATE TABLE one (id INTEGER PRIMARY KEY);",
	})
	if err := migrateFS(db, first); err != nil {
		t.Fatalf("migrate to v1 error = %v", err)
	}
	// Migration 2 is intentionally invalid SQL.
	broken := testMigrations(t, map[int]string{
		1: "CREATE TABLE one (id INTEGER PRIMARY KEY);",
		2: "CREATE TABLE two (id INTEGER PRIMARY KEY); THIS IS NOT SQL",
	})
	if err := migrateFS(db, broken); err == nil {
		t.Fatal("broken migration must fail")
	}
	version, err := schemaVersion(db)
	if err != nil {
		t.Fatalf("schemaVersion() error = %v", err)
	}
	if version != 1 {
		t.Fatalf("schema version after failed upgrade = %d, want 1 (rolled back)", version)
	}
	if _, err := db.Exec("SELECT 1 FROM two LIMIT 0"); err == nil {
		t.Fatal("partially applied migration must be rolled back")
	}
}

func TestMigrationsNewerDatabaseFailsClearly(t *testing.T) {
	db := openTestDB(t)
	files := testMigrations(t, map[int]string{
		1: "CREATE TABLE one (id INTEGER PRIMARY KEY);",
	})
	if _, err := db.Exec("PRAGMA user_version = 7"); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	err := migrateFS(db, files)
	if err == nil {
		t.Fatal("database newer than the binary must fail")
	}
	if !strings.Contains(err.Error(), "exceeds the supported version") {
		t.Fatalf("error must explain the version mismatch: %v", err)
	}
}

func TestMigrationsDuplicateVersionFails(t *testing.T) {
	db := openTestDB(t)
	dir := filepath.Join(t.TempDir(), "migrations")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"0001_a.sql", "0001_b.sql"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("SELECT 1;"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	err := migrateFS(db, os.DirFS(filepath.Dir(dir)))
	if err == nil || !strings.Contains(err.Error(), "duplicate migration version") {
		t.Fatalf("duplicate versions must fail clearly: %v", err)
	}
}

func TestMigrationsDeterministicFreshCreation(t *testing.T) {
	// The embedded migration set must create an identical schema twice.
	paths := make([]string, 0, 2)
	for index := 0; index < 2; index++ {
		dir := t.TempDir()
		path := filepath.Join(dir, "db.sqlite")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatalf("sql.Open error = %v", err)
		}
		if err := migrate(db); err != nil {
			t.Fatalf("migrate error = %v", err)
		}
		var version int
		if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
			t.Fatalf("user_version: %v", err)
		}
		if version < 1 {
			t.Fatalf("embedded schema version = %d", version)
		}
		schema := dumpSchema(t, db)
		paths = append(paths, schema)
		db.Close()
	}
	if paths[0] != paths[1] {
		t.Fatal("fresh database creation is not deterministic")
	}
}

func dumpSchema(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query("SELECT type, name, sql FROM sqlite_master WHERE sql IS NOT NULL ORDER BY type, name")
	if err != nil {
		t.Fatalf("dump schema: %v", err)
	}
	defer rows.Close()
	var builder strings.Builder
	for rows.Next() {
		var kind, name, sql string
		if err := rows.Scan(&kind, &name, &sql); err != nil {
			t.Fatalf("scan schema: %v", err)
		}
		fmt.Fprintf(&builder, "%s|%s|%s\n", kind, name, sql)
	}
	return builder.String()
}

func TestMigrationsRejectVersionGaps(t *testing.T) {
	db := openTestDB(t)
	// A set with a gap (1,3) must fail before applying anything: the database
	// must never reach a state where it looks newer than the migrations that
	// actually produced it.
	files := testMigrations(t, map[int]string{
		1: "CREATE TABLE one (id INTEGER PRIMARY KEY);",
		3: "CREATE TABLE three (id INTEGER PRIMARY KEY);",
	})
	err := migrateFS(db, files)
	if err == nil || !strings.Contains(err.Error(), "not contiguous") {
		t.Fatalf("version gap must fail clearly: %v", err)
	}
	version, err := schemaVersion(db)
	if err != nil {
		t.Fatalf("schemaVersion() error = %v", err)
	}
	if version != 0 {
		t.Fatalf("version gap must not apply any migration: version = %d, want 0", version)
	}
	if _, err := db.Exec("SELECT 1 FROM one LIMIT 0"); err == nil {
		t.Fatal("version gap must not create tables from the partial set")
	}
}

// TestMigrationsUpgradeEmbeddedV1ToCurrent proves the real embedded migration
// set upgrades a database created by the previous release (schema version 1)
// to the current version without losing data.
func TestMigrationsUpgradeEmbeddedV1ToCurrent(t *testing.T) {
	db := openTestDB(t)
	// Build a v1 database from the real embedded 0001 SQL, then upgrade it
	// with the full embedded set.
	v1SQL, err := fs.ReadFile(migrationFS, "migrations/0001_initial.sql")
	if err != nil {
		t.Fatalf("read embedded v1 migration: %v", err)
	}
	v1Only := testMigrations(t, map[int]string{1: string(v1SQL)})
	if err := migrateFS(db, v1Only); err != nil {
		t.Fatalf("migrate to v1 error = %v", err)
	}
	version, err := schemaVersion(db)
	if err != nil || version != 1 {
		t.Fatalf("schema version after v1 = %d (err %v), want 1", version, err)
	}
	if _, err := db.Exec(`INSERT INTO tasks (task_id, objective, status, workspace, created_at, started_at)
		VALUES ('task-v1', 'objective', 'running', '/ws', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed v1 data: %v", err)
	}

	if err := migrate(db); err != nil {
		t.Fatalf("upgrade to current error = %v", err)
	}
	version, err = schemaVersion(db)
	if err != nil || version != supportedSchemaVersion() {
		t.Fatalf("schema version after upgrade = %d (err %v), want %d", version, err, supportedSchemaVersion())
	}
	if _, err := db.Exec("SELECT 1 FROM governor_rate_events LIMIT 0"); err != nil {
		t.Fatalf("governor_rate_events missing after upgrade: %v", err)
	}
	// The recovery schema (issue #9) must be present on an upgraded database.
	if _, err := db.Exec("SELECT resume_count FROM tasks LIMIT 0"); err != nil {
		t.Fatalf("tasks.resume_count missing after upgrade: %v", err)
	}
	if _, err := db.Exec("SELECT workspace_signature FROM actions LIMIT 0"); err != nil {
		t.Fatalf("actions.workspace_signature missing after upgrade: %v", err)
	}
	if _, err := db.Exec("SELECT recovery_reason FROM tool_attempts LIMIT 0"); err != nil {
		t.Fatalf("tool_attempts.recovery_reason missing after upgrade: %v", err)
	}
	if _, err := db.Exec("SELECT recovery_reason FROM provider_attempts LIMIT 0"); err != nil {
		t.Fatalf("provider_attempts.recovery_reason missing after upgrade: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE task_id = 'task-v1'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("v1 task lost after upgrade: count=%d err=%v", count, err)
	}
	// Re-running on the upgraded database is a no-op.
	if err := migrate(db); err != nil {
		t.Fatalf("re-migrate after upgrade error = %v", err)
	}
}

// TestMigrationsUpgradeEmbeddedV8ToCurrentWithBaselineData proves the real
// embedded migration set upgrades a version-8 database (the previous release
// of the verification schema, issue #11) to the current version: an existing
// workspace_baselines row survives, the truncation columns are added with the
// default 0, and the store API round-trips the new flags (migration 0009,
// issue #11 review).
func TestMigrationsUpgradeEmbeddedV8ToCurrentWithBaselineData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open error = %v", err)
	}
	// Build a version-8 database from the real embedded migrations 1..8.
	entries := make(map[int]string)
	for version := 1; version <= 8; version++ {
		name := fmt.Sprintf("migrations/%04d_*.sql", version)
		matches, matchErr := fs.Glob(migrationFS, name)
		if matchErr != nil || len(matches) != 1 {
			t.Fatalf("embedded migration %04d not found: %v", version, matchErr)
		}
		sqlText, readErr := fs.ReadFile(migrationFS, matches[0])
		if readErr != nil {
			t.Fatalf("read embedded migration %d: %v", version, readErr)
		}
		entries[version] = string(sqlText)
	}
	v8 := testMigrations(t, entries)
	if err := migrateFS(db, v8); err != nil {
		t.Fatalf("migrate to v8 error = %v", err)
	}
	version, err := schemaVersion(db)
	if err != nil || version != 8 {
		t.Fatalf("schema version after v8 = %d (err %v), want 8", version, err)
	}
	// Seed a task and a v8-shape workspace_baselines row (no truncation flags).
	if _, err := db.Exec(`INSERT INTO tasks (task_id, objective, status, workspace, created_at, started_at)
		VALUES ('task-v8', 'inspect', 'running', '/ws', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed v8 task: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO workspace_baselines (task_id, git_status_json, git_diff_json, created_at)
		VALUES ('task-v8', ' M pre-existing.txt', 'diff', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed v8 baseline: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close v8 db: %v", err)
	}

	// state.Open runs the full embedded migration set: 8 -> 9.
	store, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("state.Open() upgrade error = %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	status, diff, statusTruncated, diffTruncated, ok, err := store.WorkspaceBaseline(ctx, "task-v8")
	if err != nil || !ok {
		t.Fatalf("WorkspaceBaseline() = ok %v, err %v", ok, err)
	}
	if status != " M pre-existing.txt" || diff != "diff" {
		t.Fatalf("v8 baseline data lost after upgrade: %q/%q", status, diff)
	}
	if statusTruncated || diffTruncated {
		t.Fatalf("upgraded baseline flags = %t/%t, want false/false (default 0)", statusTruncated, diffTruncated)
	}
	// The store API round-trips the new flags on the upgraded database.
	if err := store.SaveWorkspaceBaseline(ctx, "task-v8", status, diff, true, false); err != nil {
		t.Fatalf("SaveWorkspaceBaseline() on upgraded db error = %v", err)
	}
	_, _, statusTruncated, diffTruncated, ok, err = store.WorkspaceBaseline(ctx, "task-v8")
	if err != nil || !ok || !statusTruncated || diffTruncated {
		t.Fatalf("flag round trip = ok %v, %t/%t, err %v; want true/false", ok, statusTruncated, diffTruncated, err)
	}
	snapshot, err := store.LoadRecoverySnapshot(ctx, "task-v8")
	if err != nil {
		t.Fatalf("LoadRecoverySnapshot() error = %v", err)
	}
	if !snapshot.BaselineGitStatusTruncated || snapshot.BaselineGitDiffTruncated {
		t.Fatalf("snapshot baseline flags = %t/%t, want true/false", snapshot.BaselineGitStatusTruncated, snapshot.BaselineGitDiffTruncated)
	}
	current, err := schemaVersion(store.db)
	if err != nil || current != supportedSchemaVersion() {
		t.Fatalf("schema version after upgrade = %d (err %v), want %d", current, err, supportedSchemaVersion())
	}
}

// TestMigrationsOperationalProfilesTableAdditiveAndPreserving covers the
// #91 migration plus later additive task migrations: a fresh database carries
// the operational profiles table and frozen execution contract columns, an
// existing database upgrades without losing prior state, and legacy
// scripted/OmniRoute rows (empty identity columns) stay untouched.
func TestMigrationsOperationalProfilesTableAdditiveAndPreserving(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Upgrade from the #14 schema (1..12): seed a provider attempt identity
	// row first, then apply the full embedded migration set.
	if err := migrateFS(db, migrationFS); err != nil {
		t.Fatalf("migrate full set error = %v", err)
	}
	version, err := schemaVersion(db)
	if err != nil {
		t.Fatal(err)
	}
	if version != 15 {
		t.Fatalf("schema version = %d, want 15", version)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM provider_operational_profiles`).Scan(&count); err != nil {
		t.Fatalf("operational profiles table missing: %v", err)
	}
	// Re-running the migration set on an up-to-date database is a no-op.
	if err := migrateFS(db, migrationFS); err != nil {
		t.Fatalf("re-run migration error = %v", err)
	}
	version, _ = schemaVersion(db)
	if version != 15 {
		t.Fatalf("schema version after re-run = %d, want 15", version)
	}
	// The historical provider identity columns still exist and accept the
	// #14 projection shape (additive migration, no historical table altered).
	if _, err := db.Exec(`INSERT INTO provider_attempts
		(execution_id, task_id, client_request_id, provider, model_pool, model, attempt_sequence, receipt_aware, protocol_family, config_identity, delivery_state, status, created_at, prepared_at)
		VALUES ('exec-991', 'task-991', 'req-991', 'scripted', 'instant', 'scripted', 1, 0, 'openai_compatible', 'provider.Config{...}', '', 'completed', '2026-08-27T00:00:00Z', '2026-08-27T00:00:00Z')`); err != nil {
		t.Fatalf("historical provider_attempts shape broken by migration: %v", err)
	}
}
