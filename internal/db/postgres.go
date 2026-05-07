package db

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PGPool wraps pgxpool.Pool so the rest of the codebase depends on our type,
// not directly on pgx. This makes future swaps/mocking easier.
type PGPool struct {
	Pool *pgxpool.Pool
}

// NewPostgres creates a connection pool to PostgreSQL.

// We use a pool (pgxpool) rather than a single connection because our
// background worker and HTTP handlers may query concurrently. A pool
// hands out connections from a reusable set — avoids the overhead of
// opening a new TCP connection per query.
func NewPostgres(ctx context.Context) (*PGPool, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, fmt.Errorf("DATABASE_URL environment variable is not set")
	}

	// pgxpool.ParseConfig lets us tune pool settings before connecting.
	// For now we use defaults (max 4 connections), which is fine for a
	// dev/staging workload on Railway's free tier.
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to parse DATABASE_URL: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create postgres pool: %w", err)
	}

	// Ping confirms the pool can actually reach the database.
	// Fail fast at startup rather than silently accepting requests that
	// will all fail at query time.
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("postgres ping failed: %w", err)
	}

	return &PGPool{Pool: pool}, nil
}

// Close drains the pool gracefully. Call this in main() via defer.
func (p *PGPool) Close() {
	p.Pool.Close()
}

// Ping is used by the health endpoint to verify DB connectivity at runtime.
func (p *PGPool) Ping(ctx context.Context) error {
	return p.Pool.Ping(ctx)
}
