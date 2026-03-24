// internal/routing/alias.go
//
// Alias-resolution cache and middleware (with SQL fallback).
//
// Context
// -------
// Tenants in ALIAS or BOTH routing modes expose human-friendly paths —
// “/about” — that must be rewritten to their absolute component paths
// (e.g. “/content/page/view/about”).  A lightweight interface (AliasTenant)
// keeps this package independent of the tenant package, avoiding cyclic
// imports.
//
// Workflow
// --------
//   1. Tenant cold-loads an AliasCache via routing.NewAliasCache().
//   2. tenant.Router() inserts routing.Middleware(t) high in the chain.
//   3. Each request looks up r.URL.Path in the in-memory map.
//      • On hit  → rewrite and continue.
//      • On miss → one-shot SQL lookup; if found, store + rewrite.
//   4. The cache is refreshed when its TTL expires **or** when
//      site.route_version increments.
//

package routing

import (
	"context"
	"database/sql"
	"net/http"
	"sync"
	"time"

	"github.com/yanizio/adept/internal/platform/data/database"
	"go.uber.org/zap"
)

//
// AliasCache
//

// AliasCache stores alias→target pairs plus TTL and route-version state.
type AliasCache struct {
	mu       sync.RWMutex
	data     map[string]string
	loadedAt time.Time
	ttl      time.Duration
	version  int
	db       *sql.DB
}

// NewAliasCache returns an empty cache with the given TTL.
func NewAliasCache(db *sql.DB, ttl time.Duration) *AliasCache {
	return &AliasCache{
		data: make(map[string]string),
		db:   db,
		ttl:  ttl,
	}
}

// Load refreshes the entire map from route_alias.
func (c *AliasCache) Load(ctx context.Context) error {
	rows, err := c.db.QueryContext(ctx,
		`SELECT alias_path, target_path FROM route_alias`)
	if err != nil {
		return err
	}
	defer rows.Close()

	fresh := make(map[string]string)
	for rows.Next() {
		var alias, target string
		if err := rows.Scan(&alias, &target); err != nil {
			return err
		}
		fresh[alias] = target
	}
	if err := rows.Err(); err != nil {
		return err
	}

	c.mu.Lock()
	c.data = fresh
	c.loadedAt = time.Now()
	c.mu.Unlock()

	zap.L().Debug("alias cache loaded",
		zap.Int("count", len(fresh)))
	return nil
}

// lookup returns (target,true) on cache hit and not-stale; otherwise ("",false).
func (c *AliasCache) lookup(path string) (string, bool) {
	c.mu.RLock()
	tgt, ok := c.data[path]
	stale := time.Since(c.loadedAt) > c.ttl
	c.mu.RUnlock()
	return tgt, ok && !stale
}

// store inserts a single alias→target into the map (used after SQL fallback).
func (c *AliasCache) store(path, target string) {
	c.mu.Lock()
	c.data[path] = target
	if c.loadedAt.IsZero() {
		c.loadedAt = time.Now()
	}
	c.mu.Unlock()
}

// needsRefresh returns true when TTL expired or route_version changed.
func (c *AliasCache) needsRefresh(curVer int) bool {
	c.mu.RLock()
	stale := time.Since(c.loadedAt) > c.ttl || curVer != c.version
	c.mu.RUnlock()
	return stale
}

func (c *AliasCache) setVersion(v int) { c.mu.Lock(); c.version = v; c.mu.Unlock() }

//
// Middleware factory
//

// AliasTenant keeps routing independent of tenant to avoid cyclic imports.
type AliasTenant interface {
	RoutingMode() string
	RouteVersion() int
	AliasCache() *AliasCache
}

// Routing-mode constants.
const (
	RouteModeAbsolute  = "absolute"
	RouteModeAliasOnly = "alias"
	RouteModeBoth      = "both"
)

// Middleware rewrites friendly paths to absolute component paths.
func Middleware(t AliasTenant) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

			// Skip quickly if tenant is absolute-only
			if t.RoutingMode() == RouteModeAbsolute {
				next.ServeHTTP(w, r)
				return
			}

			cache := t.AliasCache()

			// Refresh cache on TTL expiry or route_version bump
			if cache.needsRefresh(t.RouteVersion()) {
				if err := cache.Load(r.Context()); err == nil {
					cache.setVersion(t.RouteVersion())
				} else {
					zap.L().Warn("alias cache reload failed", zap.Error(err))
				}
			}

			// 1️⃣  In-memory lookup
			if tgt, ok := cache.lookup(r.URL.Path); ok {
				rewriteAndServe(w, r, tgt, next)
				return
			}

			// 2️⃣  First-hit SQL fallback
			var dbTgt string
			err := cache.db.QueryRowContext(r.Context(),
				database.Rebind(`SELECT target_path FROM route_alias
					  WHERE alias_path = ? LIMIT 1`),
				r.URL.Path,
			).Scan(&dbTgt)

			switch err {
			case nil:
				cache.store(r.URL.Path, dbTgt)
				rewriteAndServe(w, r, dbTgt, next)
				return
			case sql.ErrNoRows:
				// treat as miss
			default:
				zap.L().Warn("alias SQL fallback failed", zap.Error(err))
			}

			// Alias not found
			if t.RoutingMode() == RouteModeAliasOnly {
				http.NotFound(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// rewriteAndServe mutates the request path then delegates to next.
func rewriteAndServe(w http.ResponseWriter, r *http.Request, target string, next http.Handler) {
	orig := r.URL.Path
	r.URL.Path = target
	r.RequestURI = target
	zap.L().Debug("alias rewrite",
		zap.String("from", orig),
		zap.String("to", target))
	next.ServeHTTP(w, r)
}
