// internal/database/database.go
//
// Package database centralises sqlx connection helpers.  In addition to the
// existing dial-with-retry helpers, this revision adds a **tenant-aware Conn**
// accessor so callers (e.g., the forms “store” action) can obtain the correct
// *sqlx.DB for the current request’s tenant.  The design is intentionally thin
// so a future connection pool / multi-host setup can replace it without
// changing call sites.
//
// Key additions
//   •  Registry of tenant IDs → *sqlx.DB, protected by sync.RWMutex.
//   •  RegisterTenant(id, db) for boot-time wiring.
//   •  WithTenant(ctx, id) helper embeds the tenant string in context.
//   •  Conn(ctx) fetches the DB for ctx’s tenant, falling back to defaultDB.
//   •  InitDefault(dsn) one-liner opens a default (global) connection.
//
// Oxford commas, two-space sentence spacing, concise inline notes.
//
//------------------------------------------------------------------------------

package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/go-sql-driver/mysql" // driver side-effect
	_ "github.com/jackc/pgx/v5/stdlib" // driver side-effect
	"github.com/jmoiron/sqlx"
)

//
// Connection-pool options
//

// Options tunes pool behaviour and retry policy.  Zero values fall back to
// sensible defaults (see defaultOpts).
type Options struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	Retries         int
	RetryBackoff    time.Duration
}

var defaultOpts = Options{
	MaxOpenConns:    15,
	MaxIdleConns:    5,
	ConnMaxLifetime: 30 * time.Minute,
	Retries:         0,
	RetryBackoff:    time.Second,
}

func (dst *Options) merge() {
	if dst.MaxOpenConns == 0 {
		dst.MaxOpenConns = defaultOpts.MaxOpenConns
	}
	if dst.MaxIdleConns == 0 {
		dst.MaxIdleConns = defaultOpts.MaxIdleConns
	}
	if dst.ConnMaxLifetime == 0 {
		dst.ConnMaxLifetime = defaultOpts.ConnMaxLifetime
	}
	if dst.RetryBackoff == 0 {
		dst.RetryBackoff = defaultOpts.RetryBackoff
	}
}

//
// DSN provider glue
//

// DSNProvider returns a DSN string.  Callers may fetch secrets from Vault or
// rotate credentials dynamically.
type DSNProvider func() string

// Engine identifies which SQL dialect/driver should be used.
type Engine string

const (
	EnginePostgres  Engine = "postgres"
	EngineCockroach Engine = "cockroach"
	EngineMariaDB   Engine = "mariadb"
	EngineMySQL     Engine = "mysql"
)

var (
	activeDriver   atomic.Value // string
	activeBindType atomic.Int32 // sqlx bind kind
)

func init() {
	activeDriver.Store("pgx")
	activeBindType.Store(int32(sqlx.DOLLAR))
}

// SetEngine configures global SQL dialect details used by OpenProvider and
// Rebind().
func SetEngine(engine string) error {
	drv, bind, err := dialectFor(engine)
	if err != nil {
		return err
	}
	activeDriver.Store(drv)
	activeBindType.Store(int32(bind))
	return nil
}

// Rebind rewrites a QUESTION-bind query (`?`) for the active SQL dialect.
func Rebind(query string) string {
	return sqlx.Rebind(int(activeBindType.Load()), query)
}

func dialectFor(engine string) (driver string, bindType int, err error) {
	switch Engine(strings.ToLower(strings.TrimSpace(engine))) {
	case EnginePostgres, EngineCockroach:
		return "pgx", sqlx.DOLLAR, nil
	case EngineMariaDB, EngineMySQL:
		return "mysql", sqlx.QUESTION, nil
	default:
		return "", 0, fmt.Errorf("database: unsupported engine %q", engine)
	}
}

//
// Public dial helpers (unchanged interface)
//

func Open(dsn string) (*sqlx.DB, error) {
	return OpenProvider(context.Background(), func() string { return dsn }, Options{})
}

func OpenProvider(ctx context.Context, dsn DSNProvider, opts Options) (*sqlx.DB, error) {
	return openWithOptions(ctx, dsn, opts)
}

func OpenWithPool(dsn string, maxOpen, maxIdle int) (*sqlx.DB, error) {
	return OpenProvider(
		context.Background(),
		func() string { return dsn },
		Options{MaxOpenConns: maxOpen, MaxIdleConns: maxIdle},
	)
}

// Deprecated: migrate to OpenProvider for rotational credentials.
func OpenWithOptions(ctx context.Context, dsn string, opts Options) (*sqlx.DB, error) {
	return openWithOptions(ctx, func() string { return dsn }, opts)
}

//
// Internal dial + retry loop
//

func openWithOptions(ctx context.Context, dsn DSNProvider, opts Options) (*sqlx.DB, error) {
	opts.merge()

	var lastErr error
	backoff := opts.RetryBackoff

	for attempt := 0; attempt <= opts.Retries; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		drv, _ := activeDriver.Load().(string)
		if drv == "" {
			drv = "pgx"
		}
		db, err := sqlx.Open(drv, dsn())
		if err != nil {
			lastErr = err
			goto retry
		}

		db.SetMaxOpenConns(opts.MaxOpenConns)
		db.SetMaxIdleConns(opts.MaxIdleConns)
		db.SetConnMaxLifetime(opts.ConnMaxLifetime)

		if err = db.PingContext(ctx); err == nil {
			return db, nil
		}

		_ = db.Close()
		lastErr = err

	retry:
		if attempt < opts.Retries {
			select {
			case <-time.After(backoff):
				backoff *= 2
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}

	if lastErr == nil {
		lastErr = errors.New("database: open failed with unknown error")
	}
	return nil, lastErr
}

//
// Tenant registry
//

var (
	regMu     sync.RWMutex
	tenantDB  = make(map[string]*sqlx.DB)
	defaultDB *sqlx.DB
)

type ctxKey struct{}

// WithTenant returns a child context tagged with tenantID.
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, ctxKey{}, tenantID)
}

// RegisterTenant associates tenantID with db.  Call once during tenant
// bootstrap (e.g., after migrations).
func RegisterTenant(tenantID string, db *sqlx.DB) {
	regMu.Lock()
	defer regMu.Unlock()
	tenantDB[tenantID] = db
}

// InitDefault sets the global fallback connection used when ctx has no tenant.
func InitDefault(db *sqlx.DB) { defaultDB = db }

// Conn returns the *sqlx.DB for the current tenant, or defaultDB when the
// context is not tenant-scoped.  It returns a zero sqlx.DB pointer if neither
// is registered, allowing callers to check for nil.
func Conn(ctx context.Context) *sqlx.DB {
	if ctx != nil {
		if id, ok := ctx.Value(ctxKey{}).(string); ok {
			regMu.RLock()
			db := tenantDB[id]
			regMu.RUnlock()
			return db
		}
	}
	return defaultDB
}

//
// Utility shim for sql.DB compat callers (rare)
//

// Raw returns the underlying *sql.DB to interoperate with packages that do not
// understand sqlx.
func Raw(db *sqlx.DB) *sql.DB {
	if db == nil {
		return nil
	}
	return db.DB
}
