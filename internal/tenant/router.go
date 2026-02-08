// internal/tenant/router.go
//
// Cached per-tenant router.
//
// Each Tenant lazily builds an in-memory chi.Router on first use and caches it
// in `t.router`.  The router mounts only those Components that are enabled for
// the tenant (via `component_acl`) and wires middleware in the following order:
//
//   1. **alias-rewrite** – rewrites friendly URLs → absolute component paths
//   2. **request-info**  – enriches the context with GeoIP / UA hints
//   3. **component routes** – mounts each enabled Component at “/”
//   4. **NotFound**      – final fallback renders home.html or 404
//
// All per-request logging or analytics will be handled later by the analytics
// package; no experimental middleware is referenced here.
//

package tenant

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/yanizio/adept/internal/component"
	"github.com/yanizio/adept/internal/requestinfo"
	"github.com/yanizio/adept/internal/routing"
)

const (
	RouteModeAbsolute  = routing.RouteModeAbsolute
	RouteModeAliasOnly = routing.RouteModeAliasOnly
	RouteModeBoth      = routing.RouteModeBoth
)

// Router builds (once) and returns the http.Handler for this tenant.
func (t *Tenant) Router() http.Handler {
	t.routerOnce.Do(func() {
		r := chi.NewRouter()

		// ---------------------------------------------------------------------
		// 1. Alias rewrite → absolute path.
		// ---------------------------------------------------------------------
		r.Use(routing.Middleware(t))

		// ---------------------------------------------------------------------
		// 2. Enrich request context (GeoIP, UA family, etc.).
		// ---------------------------------------------------------------------
		r.Use(requestinfo.Enrich)

		// ---------------------------------------------------------------------
		// 3. Mount each enabled Component.
		// ---------------------------------------------------------------------
		enabled := t.fetchEnabledComponents(context.Background())
		if len(enabled) == 0 {
			zap.L().Warn("component_acl empty – mounting all components",
				zap.String("host", t.Host()))
			enabled = component.AllNames()
		}

		for _, c := range component.All() {
			if _, ok := enabled[c.Name()]; ok {
				r.Mount("/", c.Routes())
			}
		}

		// ---------------------------------------------------------------------
		// 4. Fallback – render home page or plain 404.
		// ---------------------------------------------------------------------
		r.NotFound(func(w http.ResponseWriter, req *http.Request) {
			err := t.Renderer.ExecuteTemplate(w, "home.html",
				map[string]any{"Config": t.Config})
			if err != nil {
				http.NotFound(w, req)
			}
		})

		t.router = r
	})
	return t.router
}

//
// helpers
//

var _ sync.Once // keep go vet happy if routerOnce defined elsewhere

// fetchEnabledComponents returns a set[name] for components enabled in ACL.
func (t *Tenant) fetchEnabledComponents(ctx context.Context) map[string]struct{} {
	db := t.GetDB()
	if db == nil {
		return nil
	}

	rows, err := db.QueryContext(ctx,
		`SELECT component FROM component_acl WHERE enabled = TRUE`)
	if err != nil {
		if isUnknownTable(err) {
			return nil // ACL table not yet migrated—treat as “all enabled”.
		}
		zap.L().Error("component_acl query failed", zap.Error(err))
		return nil
	}
	defer rows.Close()

	set := make(map[string]struct{}, 8)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			zap.L().Error("component_acl scan", zap.Error(err))
			return nil
		}
		set[name] = struct{}{}
	}
	return set
}

// isUnknownTable recognises Cockroach/Postgres (42P01) “table does not exist”
// errors without importing driver-specific types.
func isUnknownTable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "42P01")
}
