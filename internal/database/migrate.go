package database

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openconvo/openconvo/migrations"
)

// advisoryLockID guards concurrent migration runs (e.g. two containers
// starting at once). Arbitrary but stable; spells "opencnvo" in hex.
const advisoryLockID int64 = 0x6f70656e636e766f

var migrationFilePattern = regexp.MustCompile(`^(\d+)_(.+)\.sql$`)

// Migration is a single schema migration loaded from the embedded
// migrations directory.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// AppliedMigration describes a migration recorded in schema_migrations.
type AppliedMigration struct {
	Version   int
	Name      string
	AppliedAt string
}

// LoadMigrations parses the embedded migration files, sorted by version.
func LoadMigrations() ([]Migration, error) {
	return loadMigrationsFrom(migrations.FS)
}

func loadMigrationsFrom(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}

	var out []Migration
	seen := map[int]string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		m := migrationFilePattern.FindStringSubmatch(entry.Name())
		if m == nil {
			continue
		}
		version, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("migration %s: invalid version: %w", entry.Name(), err)
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("duplicate migration version %d (%s and %s)", version, prev, entry.Name())
		}
		seen[version] = entry.Name()

		sql, err := fs.ReadFile(fsys, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		out = append(out, Migration{Version: version, Name: m[2], SQL: string(sql)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Migrate applies all pending migrations and returns how many were
// applied. It is safe to call concurrently from multiple processes: an
// advisory lock serializes runners, and each migration runs in its own
// transaction.
func Migrate(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	all, err := LoadMigrations()
	if err != nil {
		return 0, err
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", advisoryLockID); err != nil {
		return 0, fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		// Best-effort unlock; the lock is also released when the session ends.
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", advisoryLockID)
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     integer PRIMARY KEY,
			name        text NOT NULL,
			applied_at  timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return 0, fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[int]bool{}
	rows, err := conn.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return 0, fmt.Errorf("read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return 0, err
		}
		applied[v] = true
	}
	rows.Close()
	if rows.Err() != nil {
		return 0, rows.Err()
	}

	count := 0
	for _, m := range all {
		if applied[m.Version] {
			continue
		}
		if err := applyOne(ctx, conn.Conn(), m); err != nil {
			return count, fmt.Errorf("migration %04d_%s: %w", m.Version, m.Name, err)
		}
		count++
	}
	return count, nil
}

func applyOne(ctx context.Context, conn *pgx.Conn, m Migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, m.SQL); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO schema_migrations (version, name) VALUES ($1, $2)",
		m.Version, m.Name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SchemaVersion returns the highest applied migration version, or 0 when
// no migrations have been applied (including when schema_migrations does
// not exist yet).
func SchemaVersion(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var version *int
	err := pool.QueryRow(ctx, "SELECT max(version) FROM schema_migrations").Scan(&version)
	if err != nil {
		var pgErr *pgconn.PgError
		// 42P01 = undefined_table: migrations have never run.
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			return 0, nil
		}
		return 0, err
	}
	if version == nil {
		return 0, nil
	}
	return *version, nil
}
