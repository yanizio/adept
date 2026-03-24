// internal/tenant/entry.go
//
// Tenant cache entry and aggregate.
//
// A live Tenant bundles everything needed to serve one site: its `site` row,
// per-site DB pool, in-memory config, active Theme, Vault client, renderer,
// alias-route cache, and a lazily built chi.Router.

package tenant

import (
	"html/template"
	"net/http"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/yanizio/adept/internal/platform/data/vault"
	"github.com/yanizio/adept/internal/routing"
	"github.com/yanizio/adept/internal/tenant/meta"
	"github.com/yanizio/adept/internal/theme"
)

//
// Cache wrapper
//

type entry struct {
	tenant   *Tenant
	lastSeen int64 // UnixNano
}

//
// Tenant aggregate
//

type Tenant struct {
	// Core site objects
	Meta     meta.Record        // Row from `site`
	Config   map[string]string  // site_config key→value
	API      map[string]string  // provider API keys (openai, sendgrid, etc.)
	DB       *sqlx.DB           // Per-site connection pool
	Theme    *theme.Theme       // Active theme
	Renderer *template.Template // Convenience alias: Theme.Renderer
	Vault    *vault.Client      // Vault client for secret lookup

	// Routing data
	aliasCache *routing.AliasCache
	routeMode  string // "absolute" | "alias" | "both"
	routeVer   int    // site.route_version
	host       string // canonical host, used in logs

	// Cached chi.Router
	routerOnce sync.Once
	router     http.Handler
}

// ---------------------------- helpers used elsewhere -------------------------

func (t *Tenant) Host() string { return t.host }

// APIKey returns the API key for a provider, if configured.
func (t *Tenant) APIKey(provider string) string {
	if t == nil || t.API == nil {
		return ""
	}
	return t.API[provider]
}

// AliasCache returns the per-tenant cache, creating it on first use.
func (t *Tenant) AliasCache() *routing.AliasCache {
	if t.aliasCache == nil {
		// Default TTL 5 min; tweak via config later.
		t.aliasCache = routing.NewAliasCache(t.DB.DB, 5*time.Minute)
	}
	return t.aliasCache
}

func (t *Tenant) RoutingMode() string { return t.routeMode }
func (t *Tenant) RouteVersion() int   { return t.routeVer }

// component.TenantInfo implementations
func (t *Tenant) GetDB() *sqlx.DB              { return t.DB }
func (t *Tenant) GetConfig() map[string]string { return t.Config }
func (t *Tenant) GetTheme() *theme.Theme       { return t.Theme }
func (t *Tenant) GetVault() *vault.Client      { return t.Vault }

// Close is called by the cache evictor on idle or LRU eviction.
func (t *Tenant) Close() error { return t.DB.Close() }
