package database_test

import (
	"context"
	"strings"
	"testing"

	"github.com/openconvo/openconvo/internal/database"
	"github.com/openconvo/openconvo/internal/testutil"
)

func TestConnectReadOnlyRejectsWrites(t *testing.T) {
	writePool := testutil.NewDB(t)
	ctx := context.Background()

	readPool, err := database.ConnectReadOnly(ctx, writePool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer readPool.Close()
	if readPool.Config().MaxConns != 4 {
		t.Errorf("read-only MaxConns = %d, want 4", readPool.Config().MaxConns)
	}

	var setting string
	if err := readPool.QueryRow(ctx, `SHOW default_transaction_read_only`).Scan(&setting); err != nil {
		t.Fatal(err)
	}
	if setting != "on" {
		t.Fatalf("default_transaction_read_only = %q, want on", setting)
	}
	if _, err := readPool.Exec(ctx, `CREATE TABLE mcp_must_not_write (id integer)`); err == nil ||
		!strings.Contains(err.Error(), "read-only") {
		t.Fatalf("write error = %v, want read-only rejection", err)
	}
}

func TestMigrationsAreWellFormed(t *testing.T) {
	// Pure test: runs without a database.
	migrations, err := database.LoadMigrations()
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("no migrations found")
	}
	for i, m := range migrations {
		if m.SQL == "" {
			t.Errorf("migration %d has empty SQL", m.Version)
		}
		if i > 0 && migrations[i-1].Version >= m.Version {
			t.Errorf("migrations out of order: %d then %d", migrations[i-1].Version, m.Version)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	// testutil.NewDB already ran all migrations once.
	pool := testutil.NewDB(t)
	ctx := context.Background()

	applied, err := database.Migrate(ctx, pool)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if applied != 0 {
		t.Errorf("second run applied %d migrations, want 0", applied)
	}

	version, err := database.SchemaVersion(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if version < 1 {
		t.Errorf("schema version = %d, want >= 1", version)
	}
}

// 0002 exists to undo a false verdict: downloads that compared their
// bytes against the size in the message metadata and called a whole file
// corrupt. It has to be surgical — offering the wrong rows back to the
// sweep would re-fetch files the operator is not asking for — so this
// pins which rows it touches and which it leaves alone.
func TestRetrySizeMismatchMigrationTouchesOnlyItsOwnVerdict(t *testing.T) {
	pool := testutil.NewDB(t)
	ctx := context.Background()

	migrations, err := database.LoadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var sql string
	for _, m := range migrations {
		if m.Version == 2 {
			sql = m.SQL
		}
	}
	if sql == "" {
		t.Fatal("migration 0002 not found")
	}

	var messageID string
	if err := pool.QueryRow(ctx, `
		WITH c AS (
			INSERT INTO communities (source, external_id, name)
			VALUES ('discord', 'g1', 'guild') RETURNING id
		), ch AS (
			INSERT INTO channels (community_id, external_id, kind, name)
			SELECT c.id, 'c1', 'text', 'general' FROM c RETURNING id
		)
		INSERT INTO messages (channel_id, external_id, source_created_at)
		SELECT ch.id, 'm1', now() FROM ch
		RETURNING id::text`).Scan(&messageID); err != nil {
		t.Fatal(err)
	}

	add := func(externalID, status, reason string) string {
		t.Helper()
		var id string
		var errText *string
		if reason != "" {
			errText = &reason
		}
		if err := pool.QueryRow(ctx, `
			INSERT INTO attachments (message_id, external_id, filename, download_status, download_error)
			VALUES ($1::uuid, $2, 'file.jpg', $3, $4)
			RETURNING id::text`, messageID, externalID, status, errText).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	bug := add("a1", "failed", "size mismatch: stored 2470715 bytes, expected 737672")
	gone := add("a2", "failed", "file is no longer available at source (HTTP 404)")
	oversize := add("a3", "failed", "declared size 200000000 bytes is above the 104857600 byte limit")
	stored := add("a4", "stored", "")
	pending := add("a5", "pending", "")

	if _, err := pool.Exec(ctx, sql); err != nil {
		t.Fatalf("apply migration 0002: %v", err)
	}

	statusOf := func(id string) string {
		t.Helper()
		var status string
		if err := pool.QueryRow(ctx,
			`SELECT download_status FROM attachments WHERE id = $1::uuid`, id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		return status
	}

	if got := statusOf(bug); got != "pending" {
		t.Errorf("size-mismatch attachment = %q, want pending", got)
	}
	var reason *string
	if err := pool.QueryRow(ctx,
		`SELECT download_error FROM attachments WHERE id = $1::uuid`, bug).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != nil {
		t.Errorf("download_error = %q, want NULL beside a pending attachment", *reason)
	}

	for name, id := range map[string]string{"gone at source": gone, "oversize": oversize} {
		if got := statusOf(id); got != "failed" {
			t.Errorf("%s attachment = %q, want failed", name, got)
		}
	}
	if got := statusOf(stored); got != "stored" {
		t.Errorf("stored attachment = %q, want stored", got)
	}
	if got := statusOf(pending); got != "pending" {
		t.Errorf("pending attachment = %q, want pending", got)
	}
}

func TestSchemaVersionOnEmptyDatabase(t *testing.T) {
	pool := testutil.NewDB(t)
	ctx := context.Background()

	// Drop the bookkeeping table to simulate a never-migrated database.
	if _, err := pool.Exec(ctx, "DROP TABLE schema_migrations"); err != nil {
		t.Fatal(err)
	}
	version, err := database.SchemaVersion(ctx, pool)
	if err != nil {
		t.Fatalf("SchemaVersion on missing table: %v", err)
	}
	if version != 0 {
		t.Errorf("version = %d, want 0", version)
	}
}
