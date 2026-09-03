// Package database manages the PostgreSQL connection pool and schema
// migrations. PostgreSQL is OpenConvo's only required datastore: it
// holds the canonical archive, the full-text search index, and the
// background job queue.
package database

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect opens a pgx connection pool and verifies connectivity.
// It retries for a short period so that starting OpenConvo alongside a
// PostgreSQL container that is still booting works without operator
// intervention.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	return connect(ctx, url, false)
}

// ConnectReadOnly opens a pool whose PostgreSQL sessions reject writes. It is
// used by reader-only process boundaries such as the local MCP server, so an
// implementation mistake cannot turn a read tool into an archive write path.
func ConnectReadOnly(ctx context.Context, url string) (*pgxpool.Pool, error) {
	return connect(ctx, url, true)
}

func connect(ctx context.Context, url string, readOnly bool) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	if readOnly {
		// RuntimeParams are sent whenever the pool opens a connection, unlike a
		// one-off SET executed on whichever connection happens to be checked out.
		cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
		// Reader processes expose one bounded search operation. Four connections
		// leave room for concurrent calls without letting each MCP process claim a
		// CPU-sized share of a self-hoster's PostgreSQL connection budget.
		cfg.MaxConns = 4
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	const (
		maxWait  = 30 * time.Second
		interval = time.Second
	)
	// Report the time actually spent, not maxWait: the caller's own
	// deadline or an interrupt can end the loop early, and every failure
	// keeps the last ping error, which is what tells an operator whether
	// the database refused, rejected the credentials, or does not resolve.
	start := time.Now()
	deadline := start.Add(maxWait)
	for {
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = pool.Ping(pingCtx)
		cancel()
		if err == nil {
			return pool, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			pool.Close()
			return nil, unreachable(start, err)
		}
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			pool.Close()
			return nil, unreachable(start, err)
		}
	}
}

// unreachable wraps the last connection failure with how long Connect
// waited before giving up.
func unreachable(start time.Time, err error) error {
	return fmt.Errorf("database unreachable after %s: %w",
		time.Since(start).Round(time.Second), err)
}

// Ping verifies database connectivity with a short timeout. Used by the
// health endpoint.
func Ping(ctx context.Context, pool *pgxpool.Pool) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return pool.Ping(ctx)
}
