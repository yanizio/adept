// internal/tenant/meta/config.go
//
// Per-site configuration fetcher.
//
// Context
// -------
// Every tenant (site) can define arbitrary string settings in the
// `site_config` table.  When a tenant is cold-loaded we execute a single
// query to pull all key-value pairs, then store them in memory alongside
// the `Tenant` struct.  The map is thus immutable for the lifetime of the
// tenant cache entry, eliminating per-request SQL traffic.
//
// Workflow
// --------
//  1. `ConfigBySite` receives a `context.Context`, a *sqlx.DB pool that is
//     already in tenant scope, and the `site_id` primary key.
//  2. It runs one `SELECT "key", value FROM site_config WHERE site_id = $1`.
//  3. The result rows are copied into a slice of tiny structs, then folded
//     into a `map[string]string`.
//  4. The populated map is returned to the caller, ready for in-process
//     caching.
//
// Notes
// -----
//   - String keys are case-sensitive and expected to be unique per site.
//   - The helper never logs; callers should wrap errors with context if they
//     need more detail.
//   - Oxford commas, two spaces after periods, no m-dash per Adept style.
package meta

import (
	"context"

	"github.com/jmoiron/sqlx"
)

// ConfigBySite loads all rows from `site_config` for a single site_id and
// returns them as a map[key]value.  The query is expected to be called
// once at tenant warm-up and the result cached.
func ConfigBySite(ctx context.Context, db *sqlx.DB, siteID uint64) (map[string]string, error) {
	const q = `
	    SELECT  "key", value
	    FROM    site_config
	    WHERE   site_id = $1`

	// Small slice cap avoids reallocations when a site uses only a handful
	// of settings.  It grows automatically for larger sites.
	rows := make([]struct {
		Key   string `db:"key"`
		Value string `db:"value"`
	}, 0, 8)

	if err := db.SelectContext(ctx, &rows, q, siteID); err != nil {
		return nil, err
	}

	cfg := make(map[string]string, len(rows))
	for _, r := range rows {
		cfg[r.Key] = r.Value
	}
	return cfg, nil
}
