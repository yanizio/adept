// internal/tenant/cache.go
//
// Tenant LRU cache.
//
// Context
// -------
// A live *Tenant* bundles its own DB pool, key-value config, Component
// router, and Theme renderer.  This cache lazily loads a tenant the first
// time its host header appears and evicts idle or least-recently-used
// entries in a background loop.
//
// Instrumentation
// ---------------
// Structured Zap spans are emitted at DEBUG / INFO / WARN / ERROR levels:
//
//   • cache hit / miss
//   • DB-backed load start / success / failure
//   • host not found
//   • idle and LRU evictions (see evictor.go)
//
// These JSON lines appear in `/logs/YYYY-MM-DD.log` and, when running in a
// TTY, on stdout.
//
// Notes
// -----
// • All public methods are concurrency-safe.
// • Oxford commas, two spaces after periods.

package tenant

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"

	"github.com/yanizio/adept/internal/metrics"
	"github.com/yanizio/adept/internal/platform/data/vault"
)

/*────────────────────────── tunables / errors ──────────────────────────────*/

const (
	IdleTTL       = 30 * time.Minute // evict tenant after this idle duration
	MaxEntries    = 100              // 0 disables size eviction
	EvictInterval = 5 * time.Minute  // evictor scan cadence
)

var ErrNotFound = errors.New("tenant not found")

/*────────────────────────────── Cache type ─────────────────────────────────*/

type Cache struct {
	globalDB    *sqlx.DB
	vault       *vault.Client
	log         *zap.SugaredLogger
	sfg         singleflight.Group // coalesces concurrent loads per host
	m           sync.Map           // host → *entry
	evictTicker *time.Ticker
	idleTTL     time.Duration
	maxEntries  int
}

// New builds a Cache and starts its background evictor goroutine.
func New(
	global *sqlx.DB,
	idleTTL time.Duration,
	maxEntries int,
	lg *zap.SugaredLogger,
	vcli *vault.Client,
) *Cache {

	c := &Cache{
		globalDB:   global,
		vault:      vcli,
		idleTTL:    idleTTL,
		maxEntries: maxEntries,
		log:        lg,
	}
	c.evictTicker = time.NewTicker(EvictInterval)
	go c.evictLoop()

	lg.Infow("tenant cache online",
		"idle_ttl_min", idleTTL.Minutes(),
		"max_entries", maxEntries,
	)
	return c
}

/*────────────────────────────── cache lookup ──────────────────────────────*/

// Get looks up host in the cache, loading it on demand.
func (c *Cache) Get(host string) (*Tenant, error) {
	lookup := resolveLookupHost(host) // alias “localhost” → real host

	// Fast path.
	if v, ok := c.m.Load(host); ok {
		ent := v.(*entry)
		atomic.StoreInt64(&ent.lastSeen, time.Now().UnixNano())
		c.log.Debugw("tenant cache hit",
			"tenant", host,
			"lookup_host", lookup,
		)
		return ent.tenant, nil
	}

	// Slow path via single-flight.
	v, err, _ := c.sfg.Do(host, func() (interface{}, error) {
		// Double-check after barrier.
		if v, ok := c.m.Load(host); ok {
			ent := v.(*entry)
			atomic.StoreInt64(&ent.lastSeen, time.Now().UnixNano())
			c.log.Debugw("tenant cache hit (after barrier)",
				"tenant", host,
				"lookup_host", lookup,
			)
			return ent.tenant, nil
		}

		start := time.Now()
		c.log.Infow("tenant loading",
			"tenant", host,
			"lookup_host", lookup,
		)

		ten, err := loadSite(context.Background(), c.globalDB, host, c.vault)
		switch {
		case err == ErrNotFound:
			c.log.Warnw("tenant not found",
				"tenant", host,
				"lookup_host", lookup,
			)
			metrics.TenantLoadErrorsTotal.Inc()
			return nil, err
		case err != nil:
			c.log.Errorw("tenant load error",
				"tenant", host,
				"lookup_host", lookup,
				"err", err,
			)
			metrics.TenantLoadErrorsTotal.Inc()
			return nil, err
		}

		ent := &entry{tenant: ten, lastSeen: time.Now().UnixNano()}
		c.m.Store(host, ent)

		c.log.Infow("tenant online",
			"tenant", host,
			"lookup_host", lookup,
			"load_ms", time.Since(start).Milliseconds(),
		)
		metrics.TenantLoadTotal.Inc()
		metrics.ActiveTenants.Inc()
		return ten, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*Tenant), nil
}
