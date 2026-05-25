package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// FeatureFlags reads boolean flags from the feature_flags DB table with a 60s
// in-memory cache so changes propagate without a service restart.
type FeatureFlags struct {
	mu          sync.RWMutex
	flags       map[string]string
	lastFetched time.Time
	ttl         time.Duration
	pool        *pgxpool.Pool
}

func NewFeatureFlags(pool *pgxpool.Pool) *FeatureFlags {
	return &FeatureFlags{
		flags: make(map[string]string),
		ttl:   60 * time.Second,
		pool:  pool,
	}
}

// Get returns the raw string value for key, or "" if missing.
// Refreshes from DB when the cache is stale.
func (f *FeatureFlags) Get(ctx context.Context, key string) string {
	f.mu.RLock()
	age := time.Since(f.lastFetched)
	val := f.flags[key]
	f.mu.RUnlock()

	if age < f.ttl && f.lastFetched != (time.Time{}) {
		return val
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	// Double-check after acquiring write lock
	if time.Since(f.lastFetched) < f.ttl && f.lastFetched != (time.Time{}) {
		return f.flags[key]
	}

	rows, err := f.pool.Query(ctx, `SELECT key, value FROM feature_flags`)
	if err != nil {
		slog.Warn("feature flags refresh failed", "error", err)
		return f.flags[key]
	}
	defer rows.Close()

	fresh := make(map[string]string)
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) == nil {
			fresh[k] = v
		}
	}
	if rows.Err() == nil {
		f.flags = fresh
		f.lastFetched = time.Now()
	}

	return f.flags[key]
}

// Bool returns true when the flag value is exactly "true".
func (f *FeatureFlags) Bool(ctx context.Context, key string) bool {
	return f.Get(ctx, key) == "true"
}
