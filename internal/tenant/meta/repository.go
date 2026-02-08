// internal/tenant/meta/query.go
//
// Site-table query helpers (dsn column removed).
//
// Context
// -------
// These functions provide read-only access to the **site** table for
// operations that sit *outside* the main HTTP bootstrap path:
//
//   • `AllActive` — admin dashboards, cron jobs, batch reports.
//   • `ByHost`    — tenant loader on first request.
//
// The DSN column has been dropped from the schema — per-tenant passwords
// now come from Vault and the DSN is built at runtime.  Only non-secret
// columns remain in the SELECT list.
//
// Notes
// -----
//   • Column list matches the fields in `meta.Record`; update both together.
//   • `dsn` has been removed; make sure the `Record` struct no longer
//     contains that field (or tag it with `sqlx:"-"` if legacy code still
//     references it).
//   • Oxford commas, two spaces after periods.

package meta

import (
	"context"
	"log"

	"github.com/jmoiron/sqlx"
)

//
// AllActive
//

// AllActive returns every site that is neither suspended nor deleted.
// Intended for admin dashboards or batch operations, not the HTTP
// bootstrap path.
func AllActive(db *sqlx.DB) ([]Record, error) {
	const q = `
        SELECT id, host, theme, locale, routing_mode, route_version,
               preload, suspended_at, deleted_at, created_at, updated_at
        FROM   site
        WHERE  suspended_at IS NULL
          AND  deleted_at   IS NULL`
	var rows []Record
	if err := db.Select(&rows, q); err != nil {
		return nil, err
	}
	return rows, nil
}

//
// ByHost
//

// ByHost fetches a single site row that is not suspended or deleted.  The
// lookup respects request deadlines via the supplied context.Context.
func ByHost(ctx context.Context, db *sqlx.DB, host string) (*Record, error) {
	const q = `
        SELECT id, host, theme, locale, routing_mode, route_version,
               preload, suspended_at, deleted_at, created_at, updated_at
        FROM   site
        WHERE  host = $1
          AND  suspended_at IS NULL
          AND  deleted_at   IS NULL
        LIMIT  1`
	var rec Record
	if err := db.GetContext(ctx, &rec, q, host); err != nil {
		// TODO(bjy): replace stdlib log with project logger once observability
		// package lands.
		log.Printf("meta.ByHost: host=%q err=%v", host, err)
		return nil, err
	}
	return &rec, nil
}
