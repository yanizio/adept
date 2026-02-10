// internal/tenant/helpers.go
//
// Tenant helper functions shared across loader, cache, and tests.
//
// Context
// -------
// These helpers centralise logic reused by multiple tenant-related files:
//
//   • `resolveLookupHost` — maps the literal Host header “localhost” to an
//     alias defined via `ADEPT_LOCALHOST_ALIAS` or `database.localhost_alias`
//     so dev instances can masquerade as any real site row.
//
//   • `sanitizeHost`     — converts the lookup host into a canonical key
//     that doubles as the DB user name and schema.  Dots are stripped so
//     “site.yaniz.dev” → “siteyanizdev”.  Uses the result of
//     `resolveLookupHost`.
//
//   • `buildTenantDSN`   — produces the PostgreSQL DSN string given the canonical
//     key and the Vault-resolved password.
//
// Notes
// -----
// • Oxford commas, two spaces after periods.
// • No logging here; caller decides what to log.

package tenant

import (
	"fmt"
	"os"
	"strings"

	"github.com/yanizio/adept/internal/config"
)

//
// resolveLookupHost → site-row key
//

// resolveLookupHost returns the host string that should be used when
// querying the `site` table.  For "localhost" we allow an alias so local
// development can point at a real tenant without inserting an extra row.
func resolveLookupHost(h string) string {
	if h != "localhost" {
		return h
	}
	if alias := os.Getenv("ADEPT_LOCALHOST_ALIAS"); alias != "" {
		return alias
	}
	if cfg := config.Get(); cfg != nil && cfg.Database.LocalhostAlias != "" {
		return cfg.Database.LocalhostAlias
	}
	return "devlocal"
}

//
// sanitizeHost → canonical DB/user key
//

// sanitizeHost converts the lookup host to a canonical key used for
// database user name, schema, and Vault secret path.  All dots are
// removed so “api.example.dev” becomes “apiexampledev”.
func sanitizeHost(h string) string {
	return strings.ReplaceAll(resolveLookupHost(h), ".", "")
}

//
// buildTenantDSN → PostgreSQL-compatible DSN
//

// buildTenantDSN fills the canonical template:
//
//	postgres://{key}:{password}@127.0.0.1:5432/{key}?sslmode=disable
func buildTenantDSN(key, pw string) (string, error) {
	cfg := config.Get()
	if cfg == nil {
		return "", fmt.Errorf("tenant: config unavailable")
	}
	return cfg.Database.TenantDSN(key, pw)
}
