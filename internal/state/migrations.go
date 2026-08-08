package state

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrate applies all embedded versioned migrations to reach the latest
// schema version. Each migration runs inside its own SQLite transaction, and
// the schema version (PRAGMA user_version) is committed atomically with the
// migration statements, so a failed upgrade leaves the database at the
// previous version. Reopening an up-to-date database is a no-op.
func migrate(db *sql.DB) error {
	return migrateFS(db, migrationFS)
}

// migrateFS is migrate with an injectable migration filesystem so upgrade
// behavior is testable with synthetic migration sets.
func migrateFS(db *sql.DB, files fs.FS) error {
	names, err := fs.Glob(files, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("list embedded migrations: %w", err)
	}
	versions := make(map[int]string, len(names))
	for _, name := range names {
		version, parseErr := migrationVersion(name)
		if parseErr != nil {
			return parseErr
		}
		if _, exists := versions[version]; exists {
			return fmt.Errorf("duplicate migration version %d (%s)", version, name)
		}
		versions[version] = name
	}
	var sorted []int
	for version := range versions {
		sorted = append(sorted, version)
	}
	sort.Ints(sorted)
	if len(sorted) == 0 {
		return errors.New("no migrations found")
	}
	// Versions must be contiguous from 1: a gap (for example 1,3) would let a
	// partial set be applied and then make the database look newer than it is.
	for index, version := range sorted {
		if version != index+1 {
			return fmt.Errorf("migration versions are not contiguous: expected version %d, found %d", index+1, version)
		}
	}
	supported := sorted[len(sorted)-1]

	current, err := schemaVersion(db)
	if err != nil {
		return err
	}
	if current > supported {
		return fmt.Errorf("database schema version %d exceeds the supported version %d; the database was created by a newer Runstead and cannot be downgraded", current, supported)
	}
	for _, version := range sorted {
		if version <= current {
			continue
		}
		if err := applyMigration(db, files, version, versions[version]); err != nil {
			return err
		}
	}
	return nil
}

// migrationVersion extracts the leading integer from a migration file name
// such as "0001_initial.sql".
func migrationVersion(name string) (int, error) {
	base := strings.TrimSuffix(name, ".sql")
	base = strings.TrimPrefix(base, "migrations/")
	digits := base
	if index := strings.IndexByte(base, '_'); index >= 0 {
		digits = base[:index]
	}
	version, err := strconv.Atoi(digits)
	if err != nil || version <= 0 {
		return 0, fmt.Errorf("invalid migration file name %q: must start with a positive version number", name)
	}
	return version, nil
}

func applyMigration(db *sql.DB, files fs.FS, version int, name string) error {
	statement, err := fs.ReadFile(files, name)
	if err != nil {
		return fmt.Errorf("read migration %s: %w", name, err)
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", name, err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(string(statement)); err != nil {
		return fmt.Errorf("apply migration %s: %w", name, err)
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return fmt.Errorf("record migration version %d: %w", version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", name, err)
	}
	return nil
}

// schemaVersion reads PRAGMA user_version.
func schemaVersion(db *sql.DB) (int, error) {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}
