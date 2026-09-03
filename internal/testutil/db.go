// Package testutil provides shared test helpers. Database tests run
// against a real PostgreSQL instance when TEST_DATABASE_URL is set and
// are skipped otherwise; `make test-db` starts an ephemeral PostgreSQL
// container and runs the full suite.
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openconvo/openconvo/internal/database"
)

// NewDB creates a dedicated, fully migrated PostgreSQL database for one
// test and returns a pool connected to it. The database is dropped when
// the test finishes. Skips the test when TEST_DATABASE_URL is unset.
func NewDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database test (use `make test-db`)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	adminCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	admin, err := pgxpool.NewWithConfig(ctx, adminCfg)
	if err != nil {
		t.Fatalf("connect to test database server: %v", err)
	}

	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("generate database name: %v", err)
	}
	name := fmt.Sprintf("openconvo_test_%d_%s", time.Now().UnixMilli(), hex.EncodeToString(suffix))

	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close()
		t.Fatalf("create test database: %v", err)
	}

	// Registered as soon as the database exists, before it can be
	// connected to or migrated: a failure below must still drop it, or
	// every failing run leaves a database behind on the server.
	var pool *pgxpool.Pool
	t.Cleanup(func() {
		if pool != nil {
			pool.Close()
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx,
			"DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Logf("drop test database %s: %v", name, err)
		}
		admin.Close()
	})

	testCfg := adminCfg.Copy()
	testCfg.ConnConfig.Database = name
	pool, err = pgxpool.NewWithConfig(ctx, testCfg)
	if err != nil {
		t.Fatalf("connect to test database %s: %v", name, err)
	}

	if _, err := database.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}

	return pool
}
