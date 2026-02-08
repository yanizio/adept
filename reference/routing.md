Great — I’ll produce a comprehensive technical specification document covering the entire routing system in the `yanizio/adept` repo. This will include HTTP routing structure, route aliases, dynamic route registration, redirects, public/admin/internal distinctions, database schema references, and integration with widgets and components.

I’ll let you know as soon as the document is ready for your review.


# Adept Routing System Technical Specification

## System Overview

Adept’s routing system is a **multi-tenant, hierarchical HTTP router** built on the Go-Chi framework. It routes incoming requests based on the request’s host (tenant) and URL path, applying middleware in a defined sequence before dispatching to the appropriate handler. At a high level, the request flow is:

1. **Tenant Resolution:** The server uses the HTTP Host header to identify the tenant site. The `cmd/web` entrypoint maintains a **tenant cache** that lazily loads a `Tenant` for each unique host. If the host is not found in the global `site` table, the request is dropped with a 404 Not Found. Otherwise, the tenant’s in-memory `chi.Router` is retrieved or built on the fly and used to handle the request.

2. **Global Middleware:** Before reaching any tenant-specific logic, global middlewares are applied:

   * **Security Headers:** Every response is wrapped by a middleware that adds standard security headers (HSTS, CSP, X-Frame-Options, etc.) after the handler runs. This guarantees all outgoing responses include strict transport and content policies by default.
   * **HTTPS Enforcement:** If enabled by configuration, an `middleware.ForceHTTPS` middleware will **redirect HTTP to HTTPS** for known hosts. Requests arriving over HTTP (and not for `localhost`) are responded to with a 308 Permanent Redirect to the equivalent `https://` URL, but only if the host is recognized in the site registry. Unknown hosts fall through without redirection (and will likely 404 later).

3. **Tenant Router & Middleware Chain:** Each tenant has its own Chi router (`tenant.Router()`) which is composed of tenant-specific middleware and mounted routes:

   * **Alias Resolution:** First, an alias-rewrite middleware runs (if the tenant uses alias-based routing) to map human-friendly URLs to their underlying canonical paths. This is described in detail below.
   * **Request Info Enrichment:** Next, a `requestinfo.Enrich` middleware attaches contextual data like client IP, geolocation, user-agent details, etc., to the request context for later use by handlers. This allows downstream components to access `requestinfo.FromContext(r.Context())` for logging or logic (e.g., the Example component uses this to display request details).
   * **(Planned Rate Limiting):** A global and per-tenant rate-limiting middleware is planned to be inserted (as per the design) after alias resolution and before handler dispatch. This will enforce request quotas (e.g., using token buckets) both across the whole app and per tenant, protecting APIs from overuse.

4. **Route Matching and Handling:** After the above middleware, the request reaches the tenant’s core router which matches the URL path to a **component route handler**:

   * **Component Routes Mounting:** All enabled components for that tenant contribute their routes to the router. On tenant initialization, the system mounts each component’s route sub-tree at the root path `"/"` (or a prefix) if that component is enabled for the site. Routes are defined in code by each component (see **Route Registration** below). This means the union of all enabled components’ routes defines the tenant’s API endpoints and web pages.
   * **Public vs Protected Endpoints:** The routing system does not inherently differentiate public vs. protected routes – access control is enforced in handlers or via guard middleware. For example, a component can wrap specific routes with an ACL middleware (`acl.RequireRole` or `acl.RequirePermission`) to ensure only authorized users reach the handler (see **Middleware and Guards**).
   * **Handler Execution:** When a route is matched, the corresponding handler function executes. Handlers can render HTML pages (often via the templating system), return JSON for API calls, or proxy to internal services. They have access to tenant-scoped resources (DB, config, templates) via the tenant context. For instance, a content page handler might load content from the tenant DB and execute a template, whereas an API route might perform logic and write JSON.
   * **Fallback (404/Home):** If no defined route matches the request path, the router falls back to a special NotFound handler. By convention, Adept will attempt to render the site’s `home.html` template for unmatched paths, assuming it may be a request for the home page. If the home template is not found or the rendering fails, a 404 Not Found is returned.

Overall, the system cleanly separates **tenant routing** (per-host route sets) from global concerns (security, metrics, health checks). Each tenant’s router is isolated, so routes for one site do not clash with another, and tenants can have different route configurations, enabled features, and alias mappings without affecting each other. The use of Chi provides efficient HTTP routing with support for middleware chaining and subrouters for modular design.

## Route Aliases and Redirects

Adept supports **human-friendly URLs** through a flexible aliasing system, as well as HTTP redirects for moved content. These mechanisms allow site administrators to define simple or old URLs that map to internal route handlers:

* **Routing Modes:** Each tenant site can operate in one of three routing modes: **Absolute**, **Alias-only**, or **Both**. In *Absolute* mode, the site only recognizes “internal” component URLs (e.g. the full content path like `/content/page/view/about`) and treats any friendly URL as nonexistent. In *Alias-only* mode, the site expects only friendly URLs (e.g. `/about`) and will 404 if an alias is not defined for a requested path. *Both* mode allows both alias URLs and absolute paths to resolve. The site’s `routing_mode` is stored in the global `site` table (as a varchar, e.g. "absolute" or "alias") and is loaded into each Tenant object (`Tenant.routeMode`) at runtime.

* **Alias Definition:** An alias maps a **public-facing path** to a **target path** (usually an internal component route). Aliases are stored in the `route_alias` table in each tenant’s database, with fields `alias_path` and `target_path`. For example, an alias entry might map `/about` → `/content/page/view/about`. The routing subsystem maintains an in-memory cache of these aliases per tenant for fast lookup. On tenant startup, an `AliasCache` is created with a time-to-live (TTL) (default 5 minutes). The **alias resolution middleware** (`routing.Middleware(t)`) consults this cache on every request:

  1. If the incoming `r.URL.Path` exists in the alias map and the cache is fresh, the request path is **rewritten in-place** to the target path. The next handler then sees the altered `r.URL.Path` and continues processing the “real” route. A debug log is emitted for the rewrite showing the from→to mapping.
  2. If the alias is not in the in-memory map, the middleware performs a one-time SQL lookup: `SELECT target_path FROM route_alias WHERE alias_path = $1`. On a hit, it stores the new alias in the cache and rewrites the request to the target. On a miss (no such alias), the behavior depends on the routing mode:

     * In Alias-only mode, a miss results in an immediate 404 Not Found.
     * In Both mode, the request is allowed to proceed with the original URL (which may itself be a direct route).
  3. The alias cache automatically **expires** after a TTL or when the site’s `route_version` changes. The `route_version` is a monotonically increasing integer in the site record that should be bumped whenever aliases are added/changed. The middleware checks `tenant.RouteVersion()` on each request, and if it has changed (or TTL elapsed), it will reload all alias mappings from the database on the next request. This ensures that edits to aliases propagate to the running server without a restart. Reloading replaces the entire alias map in one query sweep.

*Implementation:* The alias system is implemented in `internal/routing/alias.go`. The `AliasCache` holds a thread-safe map of alias→target and the last loaded timestamp. The middleware is injected high in the chain for each tenant’s router so that path rewrites happen before route matching. Notably, the rewrite is internal – the client is unaware it happened – and thus the browser URL stays as the friendly path (this is good for SEO and user experience, while the server internally serves the content at the canonical path).

* **Redirects for Moved Routes:** In addition to aliases, Adept supports **permanent redirects** via a `route_redirect` table. This table, also stored per tenant, maps an old path to a new path (fields `old_path`, `new_path`). Redirects differ from aliases in that a redirect triggers an HTTP redirect response (e.g. 301/308), telling the client’s browser to navigate to the new URL. This is typically used when content has moved – for example, if “/pricing” page is relocated to “/products/pricing”, one could insert a redirect mapping `/pricing` → `/products/pricing` so that visitors (and search engines) are automatically sent to the new URL.

  The routing system’s design anticipates a middleware similar to alias resolution that will check `route_redirect` entries. On each request, it would look for the `r.URL.Path` in a redirect map (possibly cached). If found, it would immediately issue an HTTP redirect response to the client pointing to the `new_path`. This redirect check would likely occur **before** alias rewriting (to handle deprecated paths first) or as part of the same middleware. (As of now, the codebase contains the `route_redirect` schema and plans for this feature, though a dedicated redirect middleware may still be under development – maintainers should implement it analogous to alias resolution, but using `http.Redirect` instead of internal rewrites.)

Both alias and redirect definitions can be managed by admins (e.g. via an admin UI or migration scripts). They provide flexibility in routing: aliases give clean URLs for complex internal routes, and redirects ensure continuity when URLs change. These features are crucial in a CMS-like platform where non-technical users prefer simple URLs and where content reorganizations shouldn’t break incoming links.

## Public, Admin, and Internal Routing

Adept categorizes routes into **Public**, **Admin**, and **Internal** scopes, each with different roles and handling:

* **Public Routes:** These are the primary endpoints of each tenant’s site – the web pages and APIs meant for the general audience or end-users. Public routes include things like the homepage, content pages, and JSON API endpoints exposed to the site’s users. They are defined by Components and mounted on the tenant’s router without special restrictions by default. Public routes may still require user authentication in some cases (for example, a user profile page might require login), but they are considered part of the site’s normal functionality. All component routes (except those explicitly secured) are essentially public. For instance, the Example component registers a public page at `/example` and a public API at `/api/example` – any user can hit these endpoints (the API might require an API key or login depending on implementation, but routing-wise it’s publicly reachable). Public routes are typically served on the tenant’s primary domain (e.g. `example.com/about` for a site’s “about” page).

* **Admin Routes:** Admin routes are management or configuration interfaces not intended for general users, but for site administrators or content editors. In a multi-tenant context, *admin routes* often refer to paths used to manage a tenant’s content and settings, or a platform-wide admin console. Adept’s design supports admin routes in two ways:

  * **Per-Tenant Admin Pages:** Each site can have admin pages (e.g. `/admin` section on the same host) that are only accessible to users with appropriate roles (such as “admin” or “editor” roles in that tenant). These routes would be defined by components (or a dedicated Admin component) and protected by middleware. For example, a Content component might define `/admin/posts` for managing blog posts, but wrap it with `acl.RequireRole("editor")` so that unauthorized users get a 403 Forbidden. The routing system itself doesn’t differentiate these routes except that they commonly live under an `/admin` prefix or similar, and they will often use guard middleware (see **Middleware and Guards**). The `role` and `role_acl` tables in the tenant DB back this security model – an admin route handler can call `acl.RoleAllowed(..., component="content", action="edit")` to check permissions. By convention, we mount admin sub-routes grouped together for clarity (e.g., a component’s Routes might do `r.Route("/admin", func(ar chi.Router) { ar.Use(acl.RequireRole("admin")); ar.Get("/something", adminHandler) })`).
  * **Platform Admin Interface:** The Adept roadmap calls for a global Admin UI (for system operators to manage tenants, etc.) by version 1.0. Such an interface might be served from a special host (e.g. an “admin console” running on a separate domain or port). In the current design, the main `web` server only routes recognized tenant hosts. A global admin site could be implemented either as a special tenant (e.g. a tenant entry for the admin console) or a separate service. If implemented as part of this routing system, it would likely involve recognizing an “admin” host and either bypassing the tenant lookup (since it’s not a normal tenant) or having a reserved tenant entry. **Internal note:** As of MVP 0.5, no dedicated admin server is in the code; however, maintenance tasks like listing or editing sites can be done via direct DB or future CLI. For now, consider “admin routes” mostly those within a tenant that require elevated roles.

* **Internal Routes:** These are routes that are not tied to any tenant and are used for infrastructure or internal operations. They typically run on the same server but outside the tenant routing mechanism. In Adept:

  * **Metrics Endpoint:** A Prometheus metrics endpoint is exposed at `/metrics` on the root HTTP mux. This is an internal route used by monitoring systems to scrape metrics. It is registered on the top-level `http.ServeMux` (before tenant routing) and therefore responds on all hosts (or on the bound IP address) at the path `/metrics`. In production, this would usually be firewalled or limited to internal access since it exposes server metrics.
  * **Health Checks:** The architecture design reserves paths like `/healthz` and `/ready` for health checking. The plan is that `/healthz` will always respond (once basic systems are up) and `/ready` will respond 200 only after the server has finished loading all subsystems and is ready to serve traffic. These are classic internal endpoints for orchestration (Kubernetes liveness/readiness, etc.). Although not fully implemented in code at MVP 0.5 (as of the latest commit), placeholders exist in design. In a future update, these would be handled by the global mux similar to `/metrics`, bypassing tenant routing.
  * **Other Internal APIs:** There might be other routes considered internal (for example, debug or profiling endpoints, or webhooks from providers if they’re not tenant-specific). Currently, Go’s default pprof endpoints or similar are not explicitly enabled, but if they were, they should be treated as internal-only. Also, Vault integration does not expose HTTP routes; it’s done in-process, so no Vault endpoints are served via HTTP.

Security for internal routes is typically at the network level (they are not advertised or accessible publicly). The `middleware.Security` will still add headers to them since it’s applied globally, but things like authentication are usually not applied to metrics or health checks by the app itself. If needed, one could add simple IP allowlists in middleware for these paths.

In summary, **Public routes** serve tenant content/APIs, **Admin routes** serve management functions (protected by auth), and **Internal routes** serve platform-level needs (metrics, health) outside of tenant context. The routing system architecture cleanly separates these: internal routes are handled before tenant dispatch, and admin/public routes are mostly distinguished by conventions (URL prefix, host) and middleware rather than fundamentally different mechanisms.

## Dynamic and Tenant-Aware Routing

The routing system is **tenant-aware and dynamic**, meaning it can handle multiple sites on the same server and adapt routes per tenant at runtime:

* **Tenant Isolation:** Each tenant (site) has its own database schema and its own set of route configurations. The global `site` table (in the “global” database) acts as a registry of tenants with fields like host name, theme, locale, routing\_mode, etc.. When a request comes in, the host header is used to **lookup the tenant’s record** and initialize a `Tenant` structure. This involves opening the tenant’s dedicated DB connection (using the DSN from the site record) and loading site-specific data such as config, theme templates, and the route alias cache. The router for that tenant is then built on first use.

* **On-Demand Router Construction:** Tenant routers are constructed lazily – the first time a given host is requested, the `tenant.Cache.Get(host)` will trigger a cold load via `loadSite`. Part of this load process is calling `Tenant.Router()` which sets up the Chi router with all necessary middleware and mounts (and caches it for reuse). Because each tenant is loaded on demand, the system can handle many configured sites without running them all simultaneously; unused tenants will be evicted from memory after an idle timeout (30 minutes by default) or when an LRU limit is exceeded. A background evictor goroutine cleans up idle tenant instances to free resources. This design allows dynamic addition of new tenant sites (they just need a `site` DB entry and database provisioned) without restarting the server.

* **Per-Tenant Route Variations:** The set of routes active for one tenant can differ from another’s. This is primarily managed through the **Component Access Control (ACL)**. Each tenant DB has a `component_acl` table listing which components are enabled (the table maps component name to an enabled boolean). During router build, Adept queries this table and enables only those components marked enabled. If the ACL table is empty or missing (e.g., in a newly migrated schema), the system assumes all components are enabled by default. This means you can turn off certain features on a per-site basis. For example, a site might disable the “shop” component routes entirely by marking it disabled in `component_acl`. At router build, the code iterates through all registered components and mounts only those in the allowed set. As a result, a URL path that exists in one site (because its component is enabled) may return 404 on another site that has that component disabled.

* **Tenant-Specific Data & Behavior:** Beyond route structure, tenants can vary in content and behavior. The routing layer integrates with tenant-specific settings:

  * **Aliases/Redirects per Tenant:** As described, each tenant has its own alias and redirect definitions in its own DB. These are loaded and cached independently. Tenants can have the same alias paths pointing to different targets, or completely different sets of aliases, without conflict. The alias cache is keyed by tenant, and even the TTL refresh considers each tenant’s `route_version` separately.
  * **Themes and Components:** The actual handlers often use tenant-specific data (like the active theme or site configuration). For instance, the NotFound handler tries to render the tenant’s `home.html` using the tenant’s theme templates. Another example: a “content” component might load and serve pages from the tenant’s database, so even if two tenants share the same route `/blog/post-123`, they would fetch from different tables (their own DB). Thus, the routing resolution is tenant-aware both in matching and in execution context.
  * **Dynamic Host Aliases:** The system can treat certain hostnames as aliases for a tenant. In code, `resolveLookupHost` is used to normalize hosts (for example, it might strip common prefixes or map “localhost” to a default development site). This indicates that the system might allow multiple hostnames for one site (though a formal host alias table is not present, a simple mapping for dev is). If, for instance, `www.example.com` should serve the same site as `example.com`, one would either include both in the site table pointing to the same data, or handle it via DNS. The current implementation expects each primary host to be unique in `site` table, but additional host support could be added by extending lookup logic.
  * **Eviction and Updates:** Because routers are cached, changing a site’s configuration (like enabling a new component or changing routing\_mode) will not immediately affect a running router unless the tenant is reloaded. In practice, after making such changes in the DB, one can **bump the `route_version`** to signal route changes. While `route_version` mainly flushes alias cache, a convention could be to also have the application evict or rebuild the tenant on version changes. Currently, one might need to evict the tenant manually (e.g., by letting it idle out or restarting) to pick up new component ACL changes, as there isn’t an automatic watch on `component_acl` changes. An improvement could be to increment `route_version` on any route-related changes and have the system drop and reload the tenant’s router. Maintainers should be aware of this nuance: dynamic changes are partially handled (aliases immediately, components not yet hot-reloaded in code).

In essence, Adept’s router is multi-tenant by design: it **dispatches per host** and constructs each site’s routing table dynamically based on that site’s configuration. It isolates tenant routes and data while still running under one unified server. This gives flexibility (one codebase, many sites) and control (turn features on/off per site, custom URLs per site) at the cost of a bit more complexity in caching and invalidation logic.

## Middleware and Guards

Middleware in Adept’s routing system provide cross-cutting features (security, logging, auth checks, etc.) and **guard** certain routes with conditions. They are composed in the Chi router pipeline to execute in order. Key middleware and guard mechanisms include:

* **Security Headers Middleware:** (`middleware.Security`) – As described, this global middleware automatically appends security-related HTTP headers to every response. It is added at the very top of the stack (in `main.go`) so that even 404 or error responses carry these headers. It ensures best practices (HSTS, CSP, no-sniff, etc.) without each handler needing to manage headers.

* **HTTPS Enforcement:** (`middleware.ForceHTTPS`) – Also applied globally (conditionally, based on config), this middleware checks incoming requests and redirects them to HTTPS if they arrived via HTTP. It uses the tenant cache (`cache.Get`) to verify the host is a valid site before redirecting, which prevents open redirect issues for unknown hosts. This guard ensures that, in production, all tenant sites are accessed securely. Developers should note that in local development (`localhost`), this middleware intentionally skips forcing HTTPS.

* **Request Info Enrichment:** (`requestinfo.Enrich`) – Attached to every tenant router, this middleware parses the incoming request’s headers (User-Agent, IP address, etc.) and performs geoIP lookup and user-agent parsing to produce a structured `RequestInfo`. This info (browser name, OS, device type, client IP, geolocation) is stored in the request context. Handlers can retrieve it via `requestinfo.FromContext(r.Context())` and do things like logging or dynamic content based on location. For example, the Example component’s handler uses this to display client details. This middleware should be run early (after alias rewrite but before most handlers) so that all subsequent logic has access to the enriched context. It is indeed added right after the alias middleware in the router setup.

* **Alias Rewrite Middleware:** (`routing.Middleware(AliasTenant)`) – Discussed in **Route Aliases** above, this is injected per tenant when alias mode is in use. It intercepts requests to rewrite paths and can short-circuit 404 for missing aliases. It’s critical this runs *before* route matching and other middleware that might log or handle the request, so it is the first in the `tenant.Router()` usage chain (except global middleware which already ran). By mutating `r.URL.Path`, it effectively “tricks” subsequent handlers into thinking the request was to the target path all along – this means logging and metrics will record the *target* path, not the original alias, unless those systems explicitly capture the original (currently, the code does log the alias mapping to debug).

* **Logging and Instrumentation:** There isn’t a dedicated logging middleware that wraps every request, but Adept uses structured logging via Zap throughout the request cycle. For instance, the tenant cache will log a “cache hit” or “tenant loading” event with context for each request. Similarly, alias rewrites and SQL fallbacks are logged at debug/warn levels. This distributed logging approach means that each major step emits logs rather than one monolithic logger at the end. In the future, an access log middleware could be added for unified request logging. On the metrics side, the `internal/metrics` package defines Prometheus counters/gauges for things like tenant loads (`TenantLoadTotal`, `ActiveTenants`, etc.), security rule hits, etc. These are incremented in the code at appropriate points. The Prometheus `/metrics` endpoint then exposes these. There is also an intent to integrate OpenTelemetry tracing (as noted for MVP 0.8) – when that is done, likely a middleware will start a trace/span for each incoming request and propagate it through handlers.

* **Authentication Middleware:** Authentication in Adept is currently minimal (a stub context is used to carry user ID after login). In the future, a full auth system (session cookies, JWTs, OAuth) will be integrated. At present, after a user logs in, code would call `auth.WithUser(ctx, userID)` to store the user’s ID in context. Thereafter, any handler or middleware can check `auth.UserID(ctx)` to see if a user is logged in. This is the basis for guard middleware like ACL.

* **ACL and Role-Based Guards:** The `internal/acl` package provides Chi middleware to protect routes based on user roles:

  * `acl.RequireRole("roleName")` returns a middleware that ensures a logged-in user has at least one of the specified roles. If not logged in, it sends 401 Unauthorized; if logged in but lacks the role, 403 Forbidden is returned. For example, `r.With(acl.RequireRole("admin")).Get("/admin", ...)` would restrict that route to users with the “admin” role.
  * `acl.RequirePermission(component, action)` is a finer-grained guard that checks the `role_acl` table to see if the user’s roles grant permission for a given component action. This uses the roles assigned to the user (via `user_role` table) and queries `role_acl` to see if any of the user’s roles have `permitted=TRUE` for that component/action combination. For instance, one might protect a content editing route as `r.With(acl.RequirePermission("content", "edit")).Post("/api/posts", editHandler)` to ensure only users with proper content edit rights can call it.
  * These ACL middlewares internally rely on the `auth.UserID` being present in context (to know who the user is) and on the tenant’s database to fetch roles. They use the tenant’s `GetDB()` to query `user_role` and `role_acl` at request-time. In high-traffic scenarios, some caching may be warranted, but correctness is priority. The ACL middleware is not yet heavily used in default components (since full auth is not implemented yet in MVP 0.5), but it is built and tested. As admin and secure routes roll out, these guards will be the way to enforce that only authorized users can access certain endpoints.

* **Other Middleware:** Adept can incorporate other standard Chi middleware as needed. For example, adding a **CSRF protection middleware** for form endpoints, a **rate limiting middleware** (planned as mentioned), or **logging/trace middleware**. The structure is in place to `.Use()` such middleware either globally (in `main.go`) or per-tenant (in `tenant.Router()` before mounting components). One example already present is the **prometheus instrumentation** for HTTP metrics – the code uses `promhttp.Handler()` for metrics endpoint, and could use `chi/middleware` for things like request ID or recovery if needed. The design favors explicit, purposeful middleware placement (as seen with alias and requestinfo order) to ensure proper function.

In practice, the **middleware stack** for a typical tenant request in Adept 0.5 is: Security headers → (ForceHTTPS if enabled) → Alias rewrite → Request info enrich → \[future: RateLimit] → route handler → Security headers post-write. Additionally, on specific routes, an auth/ACL middleware may wrap the handler to enforce permissions. This layered approach provides a robust and modular way to inject cross-cutting concerns and protections into the routing flow, making the system easier to maintain and extend.

## Database Tables for Routing

Several database tables (in MySQL, per the SQL migration files) define the routing configuration, aliases, and access control. Here we document the schema and purpose of each relevant table:

* **Global Database (Configuration)**:

  * **`site`** – Master list of tenant sites. Each row corresponds to one tenant and contains routing settings:

    * `host` (VARCHAR) – The primary domain name of the site (must be unique). This is used to route requests; the Host header must match one of these for the site to be served.
    * `routing_mode` (VARCHAR(6)) – Either `"absolute"`, `"alias"`, or `"both"` (in schema default is `'path'` which corresponds to alias-enabled mode). Controls whether the site uses human-friendly URLs. This value is loaded into `Tenant.routeMode` and checked by the alias middleware.
    * `route_version` (INT) – A version number for route definitions, initially 0. Incrementing this signals that aliases or routes have changed, prompting caches to refresh. The alias middleware uses this to decide when to reload the alias map.
    * Other fields like `theme`, `locale`, etc., are not directly about routing but may affect template resolution. The `dsn` field (if present in schema) points to the tenant database – used by the loader to connect to the tenant’s DB.
    * Primary key is an `id` (BIGINT), but typically the `host` is the lookup key. There’s an index on `host` for quick lookup.

  * **`site_config`** – Key-value configuration pairs per site. Not directly routing, but could include flags that influence routing (e.g., a config to override alias TTL or to enable experimental routes). Currently mainly used for general config. For routing, this is not heavily utilized except that the entire config map is made available to templates (e.g., can be used in the NotFound home rendering).

  * **`users`** (global users) and related auth tables – The `users` table lives in global DB and holds user accounts that may log into sites. While not a routing table, it interacts with routing via authentication (user IDs in context). The `users` table ties into `user_role` in tenant DBs for permissions.

* **Tenant Database (per site)**:

  * **`component_acl`** – Controls which components’ routes are active for the tenant. Columns:

    * `component` (VARCHAR PK) – Name of the component (e.g., `"content"`, `"shop"`, `"example"`).
    * `enabled` (BOOL) – If 1/TRUE, the component’s routes are included; if 0/FALSE, they are omitted.
    * This table lets you toggle features without code changes. The router build process reads this table to include/exclude mounts. If a component isn’t listed, the system assumes enabled (or if the whole table is empty, enables all as fallback). There is an index on `enabled` for potential use (e.g., query all enabled components).
  * **`route_alias`** – Stores friendly URL aliases. Columns:

    * `alias_path` (VARCHAR PK) – The public-facing path (beginning with `/`). **Example:** `/about`.
    * `target_path` (VARCHAR) – The internal route path this alias maps to. **Example:** `/content/page/view/about`. This should correspond to an actual route of some component.
    * Timestamps `created_at`, `updated_at` – For auditing (when the alias was created/changed).
    * The alias middleware queries this table (`SELECT alias_path, target_path FROM route_alias`) to load all mappings into memory. The table’s primary key on `alias_path` ensures quick lookup if doing a direct SQL query for one alias. All aliases are local to the tenant (no cross-tenant interference).
  * **`route_redirect`** – Stores permanent redirect mappings for moved paths. Columns:

    * `old_path` (VARCHAR PK) – The old URL path that should redirect. **Example:** `/old-page`.
    * `new_path` (VARCHAR) – The new URL path where clients should be sent. **Example:** `/new-page`.
    * `created_at` timestamp – When this redirect was created (useful for tracking changes).
    * This table would be read by a redirect middleware. Typically, one might index `old_path` for lookups (here it’s primary key, so already indexed). If a site has many redirects, in-memory caching (similar to alias) would be beneficial. The semantics of usage are that a request to `old_path` results in an HTTP 301/308 redirect to `new_path`, and clients then follow to the new path (which could itself be an alias or direct route).
  * **`role`** – Defines roles for RBAC. Columns:

    * `id` (BIGINT PK), `name` (VARCHAR unique) – e.g., “admin”, “editor”, “viewer”.
    * `enabled` (BOOL) – to soft-disable a role if needed.
    * `description` – free text description.
    * These are per-tenant roles; the same role name in different tenants are distinct entries (though conceptually might serve same purpose per site).
  * **`role_acl`** – Permissions matrix for roles. Columns:

    * `role_id` (FK to role), `component` (VARCHAR), `action` (VARCHAR), `permitted` (BOOL).
    * Primary key is composite (role, component, action).
    * This table says, for a given role, whether a certain action on a component is allowed. e.g., role "editor" + component "content" + action "edit" = permitted TRUE means editors can edit content.
    * The ACL middleware uses this table via `RoleAllowed()` to enforce route-level permissions.
  * **`user_role`** – Assigns global users to tenant roles. Columns:

    * `user_id` (BIGINT) – corresponds to a global `users.id` (though not a foreign key, conceptually a link).
    * `role_id` (BIGINT) – foreign key to `role.id` in this tenant DB.
    * Primary key (user\_id, role\_id) to avoid duplicates.
    * This table is consulted to find a user’s roles in the tenant, which is then checked against `role_acl` in guards.

*(Note:* The last three tables are more about security than routing, but they directly impact which “admin” or sensitive routes a user can access, so they are included for completeness. They don’t affect route matching; they affect authorization after a route is matched.)

For maintenance: When altering routing behavior (e.g., adding a new component or new alias), schema changes may be needed. For example, adding a new type of route alias might involve extending `route_alias` (currently it’s simple one-to-one mapping). Or supporting host aliases might require a new table linking multiple hostnames to one site. As of now, these tables suffice for the core alias/redirect and feature gating needs.

Database migrations for these can be found under `sql/install/` in the repository. Changes to routing logic often go hand-in-hand with schema version bumps to ensure consistency.

## Route Registration Process

This section walks through how routes are defined in code, registered with the router, and ultimately resolved for incoming requests. It ties together the developer experience of adding a route to the runtime behavior of the system.

**1. Defining Routes in a Component:** Developers create routes by implementing the `Routes() chi.Router` method in a Component. Inside this method, the component uses the Chi router API to declare HTTP route patterns and handlers. For example, a component could do the following in its `Routes()` implementation:

```go
r := chi.NewRouter()
r.Get("/login", getLoginHandler)                      // Define a GET endpoint for "/login"
r.Route("/api", func(api chi.Router) {
    api.Get("/status", apiStatusHandler)              // Define GET "/api/status" within an API sub-route
    // ... other API endpoints
})
return r
```

*Example above from component documentation.* In this snippet, the component defines a page route `/login` and an API route `/api/status`. The `chi.NewRouter()` call creates an isolated router for the component, and the routes are attached to it. The developer writes handler functions (`getLoginHandler`, `apiStatusHandler` in this example) to handle the requests.

**2. Component Registration:** Each component is a Go package (under `components/<name>/`) that registers itself with the system. In the component’s `init()` function, it calls `component.Register(&MyComponent{})` to add an instance to the global registry. For example, the Example component does:

```go
func init() {
    component.Register(&Comp{})
}
```

Where `Comp` implements the `Component` interface. Registration makes the component known to the router builder. All components must be registered on startup; typically an anonymous import of the package in `main.go` ensures the init runs (e.g., `_ "github.com/yanizio/adept/components/example"` in main).

**3. Application Startup – Building Component List:** On server start, after all components have registered, the registry holds a map of component name → Component object. The function `component.All()` can return the slice of all registered components. At this point, no tenant routers exist yet; components are just waiting to be mounted.

**4. Tenant Router Construction:** When the first request for a given tenant arrives, the system will invoke `Tenant.Router()` for that site. This happens inside the `tenant.Cache.Get` logic after loading a Tenant. The `Router()` method uses the component registry and the tenant’s `component_acl` to assemble the routes:

* It creates a new Chi router (`r := chi.NewRouter()`).
* It attaches tenant-specific middleware: first the alias rewrite (if applicable) then the request info enrichment. These are added via `r.Use(...)` calls *before* any routes are mounted.
* It queries the tenant DB for enabled components (`SELECT component FROM component_acl WHERE enabled = 1`), building a set of allowed components. If the query returns no rows (meaning all components should be enabled, or the table isn’t set up yet), it logs a warning and defaults to including all components.
* It then iterates over all registered components and mounts each allowed component’s router at the root path: `r.Mount("/", comp.Routes())` for each component that is enabled. Mounting at “/” effectively merges the component’s routes into the main router. Chi ensures that routes don’t conflict directly; if two components define the same exact path, the first mounted would match first – in practice components are expected to have distinct route prefixes or paths to avoid ambiguity.
* After mounting all components, it defines a custom `NotFound` handler on the router (for this tenant) to handle any path that wasn’t matched by the components: this is where it tries to render `home.html` as a fallback page.
* The fully constructed `http.Handler` (Chi router) is stored in `Tenant.router` and reused for subsequent requests to that tenant.

This build process is done once per tenant lifecycle. If the tenant is evicted from cache due to inactivity, a subsequent request will trigger rebuilding the router (which will include any updated configuration or newly deployed components).

**5. Handling an Incoming Request (Route Resolution):** When a request comes in:

* The HTTP server’s root handler (in `main.go`) identifies the tenant via the Host, gets the Tenant object from cache, and calls `tenant.Router().ServeHTTP(w, r)` to delegate handling.
* The request now enters the tenant’s Chi router. It passes through the middleware chain: alias rewrite (which may modify `r.URL.Path` if an alias matches), then request info enrichment (which annotates `r.Context()`), then into Chi’s routing logic.
* Chi will parse the URL path and attempt to match it against the mounted routes. It effectively checks each component’s routes in the order they were mounted. For example, if Component A has a route `/login` and Component B also has `/login` (not recommended to have duplicates), whichever component was mounted first will handle it. Typically, components use unique prefixes (like one might have all its routes under `/api/...`) to avoid collisions.
* If a match is found, Chi invokes the corresponding handler. The handler executes, performing whatever logic is needed (database queries, template rendering, etc.). It can use values from context (like `tenant := tenant.FromContext(ctx)` or `requestinfo.FromContext(ctx)` if needed). The handler writes to the `http.ResponseWriter` to send a response.
* If no route matches, Chi invokes the previously set NotFound handler, which tries to render the home page template for the site. If that template doesn’t exist or fails, it finally sends a 404.
* During handler execution, any guard middleware applied to that route will have already run. For instance, if `RequireRole("admin")` was chained, it would have executed before the handler and possibly prevented it from running if the user lacked permission.
* After the handler completes, control goes back up the middleware stack (if any post-handling logic is there; e.g., the Security middleware adds headers at this point). Finally, the response is returned to the client.

**6. Example:** To illustrate, suppose a developer adds a new route in the “content” component for editing a page, say a POST `/api/page/edit`. They implement it in `content.Routes()` with something like:

```go
r := chi.NewRouter()
r.With(acl.RequirePermission("content", "edit")).
    Post("/api/page/edit", editPageHandler)
return r
```

They register the content component as usual. Now, when a request `POST /api/page/edit` comes to a particular tenant:

* The tenant’s router (if not built yet) is built, including the content component routes (assuming `content` is enabled in ACL).
* The alias middleware runs (but this path is already a direct API path, likely no alias).
* Chi matches `/api/page/edit` to the content component’s route. The `RequirePermission("content","edit")` middleware runs, finds the user’s roles, checks the DB’s `role_acl` for content edit permission. If the user is an editor/admin, it allows the request to proceed.
* The `editPageHandler` is called and processes the edit (e.g., verifying input and updating the DB), then maybe returns a JSON success.
* The response is sent, security headers added, etc. If the user lacked permission, the middleware would have short-circuited with 403, and the handler wouldn’t run.

**7. Route Updates:** If developers modify route definitions in code (e.g., change a URL or add a new component), they must redeploy the application for changes to take effect. After deployment, new routes are registered via `init()` and will be included for any tenant router built thereafter. Running tenants would need to be evicted or the process restarted to pick up the new code (since the router is built once and cached). For content changes that affect aliases or require redirects, updating the DB tables (and bumping `route_version`) is the procedure, as described earlier.

This end-to-end process ensures that adding a route in code is straightforward, and the system takes care of integrating it into the multi-tenant environment. The key steps for maintainers are: **define route -> register component -> ensure component is enabled for relevant tenants** (perhaps via migrations or admin UI) -> **the system will serve it**.

## Integration with Components and Widgets

**Components** are the fundamental units of functionality in Adept, and they integrate tightly with the routing system. Each component can be thought of as a feature module that owns a set of routes, as well as the logic and database schema for that feature. The routing system treats components as mountable route collections:

* As described, each component provides a `Routes() chi.Router` implementation with its URL handlers. These routes can include both “page” endpoints (HTML content, often rendered via templates) and “API” endpoints (JSON or other data). There is no hard division in routing; it’s just a convention that UI pages might be at the root or simple paths and API endpoints are often grouped under `/api` prefix. The component’s routes are mounted into the tenant router, meaning a component doesn’t need to worry about host or global middleware – it only defines the relative paths within its scope, and the system gives it the proper context.

* **Component Enable/Disable:** The component ACL mechanism means some components’ routes won’t appear for certain tenants. This is a feature of the routing assembly, not something components themselves handle (they always register their routes; the router decides to mount or not). For example, if there is a “shop” component providing e-commerce routes, a tenant that doesn’t need e-commerce can turn it off in `component_acl` – those routes simply won’t be mounted. From a routing standpoint, it’s as if they don’t exist for that site. This modularity keeps the route space clean per tenant and improves security (no need to expose routes for features the site isn’t using).

* **Reverse Routing (URL Generation):** While not fully implemented yet, the architecture includes a `routerpath` or URL builder concept. The idea is to have a way to programmatically construct URLs to routes, possibly respecting alias if needed. This might involve naming routes or having a mapping from logical identifiers to actual paths. At present, developers generate URLs either by hardcoding paths or using small helpers (like the `routing.BuildPath(parent, slug)` for assembling hierarchical paths). Widgets and templates might call such helpers to link to other pages. Maintainers plan to enhance this with a more robust solution (so that if a route changes, references to it can update automatically).

**Widgets** are smaller units of reusable UI (view fragments) that can be embedded in pages. They are not directly tied to top-level routes, but their existence influences how some routes render content:

* A widget is defined under a component (e.g., `components/example/widgets/example.go` for a widget in the Example component). Each Widget implements a `Render(ctx, params) (html, policy, err)` method that returns an HTML snippet, and an `ID()` that uniquely identifies it (like `"example/example"` for the example widget). Widgets register themselves similar to components, using `widget.Register(&Widget{})` in init, but in a separate widget registry.

* **Rendering Widgets:** Widgets do not have their own HTTP routes; instead, they are invoked within templates or pages. For instance, a page template can call a widget by ID using a template helper, e.g.: `{{ widget "auth/login" (dict "limit" 5) }}` to insert the output of the `"auth/login"` widget. When this is called during template rendering, the system looks up the widget by ID in the widget registry, then calls its `Render` method to get HTML. The `Render` method is passed a context which includes the tenant and request context, so it can make tenant-specific decisions (for example, the login widget might check the session to see if user is logged in, etc.). The widget can call `internal/view.RenderToString` to render a sub-template. In our example widget, it uses `view.RenderToString(rctx, "example", "widgets/example", data)` to render the `widgets/example` template of the Example component. There is an override chain for templates (site-specific overrides can exist in the `sites/<host>/templates/widgets/...` directory if needed, falling back to theme and component defaults).

* **Routing and Widgets:** Because widgets do not have dedicated URLs, they don’t appear in the routing table. They are invoked as part of handling a page route. For example, the Example component’s `/example` page route when handling a request might include an example widget in its HTML. The widget’s content is fetched server-side during rendering. From a routing perspective, the presence of widgets is transparent – they are an implementation detail of the handler. However, one indirect effect is that a widget might make internal HTTP calls or require data: for instance, a widget could, in theory, trigger an AJAX call to an API route to load data. If so, that API route would be a normal component route subject to routing rules. But typically, widgets are rendered fully on the server side without additional requests.

* **Integration with Components:** Widgets belong to components (each widget’s ID is namespaced by component). They often serve as partials or mini-features that accompany component pages. For example, an “auth” component might have a “login” widget that can be embedded on various pages to show a login form. The component’s routes (like `/login` page) might not directly use the widget, but other components’ pages could include it. It’s a way to reuse UI across components. From the routing perspective, this just means the handler for those pages needs to have access to the widget system (which it does via the template helper). No special routing logic is required for widgets beyond ensuring the widget is registered and the template engine can call it.

* **Example:** The Example widget (`example/widgets/example.go`) collects request info (IP, user agent data) and renders it in a snippet. If the homepage template wanted to include this info, it could do `{{ widget "example/example" . }}` (passing perhaps no special params). The widget would produce HTML listing IP, country, etc. The data it uses (`tenant.Context` which includes RequestInfo) is available because the route that triggered rendering had the `requestinfo.Enrich` middleware run already, populating that context. This demonstrates how middleware, routing, and widgets tie together: the middleware provides context, the route handler calls template rendering, and the template invokes the widget, which uses the context to generate content.

In summary, **components define the routes and core logic**, and **widgets are helper views** that don’t have their own routes but plug into component pages. The routing system primarily concerns components (since they map to URLs), but it facilitates widgets by providing context and a registry lookup. When adding a new component, you typically add its routes and possibly some widgets for its UI pieces. The system will auto-mount the routes if enabled, and widgets can be used in any template once registered.

Maintaining a clear separation: use components for anything that needs its own page or API endpoint; use widgets for reusable page fragments or dynamic content within a page. This ensures the routing table doesn’t get cluttered with micro-routes for every small fragment, and instead remains focused on meaningful page/API endpoints.

## Code References and Key Components

For maintainers and developers, here is a quick reference to key files and constructs in the codebase that implement the routing system:

* **Main HTTP Entrypoint:** *`cmd/web/main.go`* – Sets up the HTTP server, global middleware, and the bridging of host-to-tenant routing. See the logic around stripping the port and fetching the tenant from cache, then calling `tenant.Router()`. Also configures `/metrics` and applies `Security` and `ForceHTTPS` middleware.

* **Tenant Cache and Loader:** *`internal/tenant/cache.go`* and *`internal/tenant/loader.go`* – Manage on-demand loading of tenants. Notably, `Cache.Get(host)` handles tenant lookup (with singleflight to avoid duplicate loads) and calls `loadSite` if a cache miss. `loadSite` (in loader.go) performs the steps to instantiate a Tenant: retrieving the site record, loading site config, building the tenant’s DB DSN and connecting, loading theme, etc.. This is where `Tenant.Meta.RoutingMode` and `RouteVersion` come from (the site record) and the Tenant’s DB is set up.

* **Tenant Router Builder:** *`internal/tenant/router.go`* – Constructs the Chi router for a tenant. The `Tenant.Router()` function is here. It attaches the alias middleware and other tenant-specific middleware, queries `component_acl` and mounts component routes, and sets the NotFound handler. This is a critical piece for how routes are organized per tenant.

* **Alias & Redirect Logic:** *`internal/routing/alias.go`* – Contains the `AliasCache` struct and the alias rewrite middleware function. It defines the routing modes and implements `Middleware(t)` which does the cache lookup, SQL fallback, and path rewrite. It also includes helpful commentary on the workflow. There isn’t a separate file for redirect logic yet; the `route_redirect` functionality would be analogous. Keep an eye on this file (or a future `redirect.go`) if implementing redirect handling – likely it would mirror alias middleware but calling `http.Redirect`.

* **Component Registry and Interface:** *`internal/component/registry.go`* – Defines the `Component` interface and holds the global registry of components. The interface specifies `Routes() chi.Router` (and other optional hooks). Each component registers via `Register()` which adds to the registry map. There are also helpers `All()` and `AllNames()` to retrieve components. This file, along with each component’s `Routes()` implementation (see `components/*/*.go`), is where new routes are introduced in code.

* **Middleware Implementations:**

  * *`internal/middleware/security.go`* – Implements the `Security` middleware adding headers.
  * *`internal/middleware/https.go`* – Implements `ForceHTTPS` redirect middleware.
  * *`internal/requestinfo/middleware.go`* – (Implied by usage, likely in `internal/requestinfo/`) Responsible for the `Enrich` middleware that populates request context with `RequestInfo`. This uses `internal/requestinfo` package’s parsing (user agent parsing via uaparse, etc.). The `requestinfo.RequestInfo` struct and `FromContext` helper allow retrieval in handlers.
  * *`internal/acl/middleware.go`* – Contains `RequireRole` and `RequirePermission` middleware definitions used to guard routes. It uses `internal/auth` (for `UserID`) and queries from `internal/acl/store.go` for role lookup.
  * *`internal/auth/context.go`* – A simple context value helper for storing user ID in context after login.

* **Database Schema Definitions:**

  * *`sql/install/postgres.global.sql`* – Creates the `site` table (and global user/auth tables). The `site` table’s columns of interest are `host`, `routing_mode`, `route_version` as described.
  * *`sql/install/postgres.tenant.sql`* – Creates `component_acl`, `route_alias`, `route_redirect`, `role`, `role_acl`, `user_role` tables. This is a great reference for the exact schema and any default constraints.

* **Themes and Rendering:** While not strictly routing, it’s worth noting *`internal/theme/manager.go`* and *`internal/view/render.go`*. The Theme manager loads template files for a tenant’s theme. The `view.Render` functions are used by handlers (and widgets) to render HTML. For example, the NotFound handler calls `t.Renderer.ExecuteTemplate(...)` which ultimately comes from the theme’s parsed templates. Routing and rendering are connected in that a route will often choose which template to render based on path or logic.

* **Example Component:** *`components/example/example.go`* – A simple reference implementation of a component with routes. It shows how to use `requestinfo` and demonstrates both an HTML and JSON route. This can serve as a guide for building new component routes.

When maintaining the routing system, changes might involve multiple pieces above. For instance, adding a new global middleware means updating `main.go` to use it (and possibly writing it under `internal/middleware`). Adding a new route alias or redirect might involve updating the SQL and the alias middleware logic. Always consider thread-safety and performance (e.g., the alias cache uses a RWMutex for safe concurrent reads).

Finally, **testing**: The `internal/routing/alias_test.go` provides unit tests for alias behavior, and `internal/acl/store_test.go` covers role and permission queries. Use these as starting points to extend tests when modifying routing logic. Chi’s own testing utilities (httptest) can be used to simulate requests through a router for end-to-end verification of routing rules.

## Examples

Below are a few concrete examples illustrating the routing system in action, with brief annotations:

### Example Route Definition (Component)

When defining routes in a component, you create a router and add paths. For instance, the code below (from the `Component` interface docs) shows a component registering a page route and an API sub-route:

```go
r := chi.NewRouter()
r.Get("/login", getLogin)                 // Public page route at "/login"
r.Route("/api", func(api chi.Router) {
    api.Get("/status", apiStatus)         // API route at "/api/status"
})
return r
```

In this example, `/login` might serve an HTML login form, and `/api/status` might serve a JSON status. These routes will be mounted into the main router for any tenant that enables this component. (From code comments: components should mount both page and API endpoints.)

### Example Route Alias Mapping

Consider a tenant that wants `/about` to show a company about page which is implemented in the Content component. Suppose the real handler is served at `/content/page/view/about`. Instead of exposing the full path, we create an alias:

* **Alias entry (database):** In the `route_alias` table, insert a row: **alias\_path = `/about`**, **target\_path = `/content/page/view/about`**.

At runtime, when a user requests `GET /about`, the alias middleware finds this mapping and transparently rewrites the request to `/content/page/view/about` before routing. The user sees `/about` in the browser, but the Content component’s handler for `/content/page/view/about` is what executes. This was shown in tests where an alias was pre-loaded:

```go
cache.store("/about", "/content/page/view/about")
...
req := httptest.NewRequest("GET", "/about", nil)
// After middleware, the request path becomes "/content/page/view/about"
```

In the alias test, they confirmed that after the middleware ran, the served path was changed to the target. This demonstrates the alias functionality.

### Example Redirect Entry

If a page moves and we want to redirect traffic, we use `route_redirect`. For example, say we have renamed `/pricing` to `/plans`. We add to `route_redirect` table: **old\_path = `/pricing`**, **new\_path = `/plans`**. When the routing logic encounters a request for `/pricing`, it will respond with an HTTP redirect:

```
Client requests: GET /pricing
Server responds: 301 Moved Permanently -> Location: /plans
Client then requests: GET /plans (new location)
```

The new path `/plans` would presumably either be an alias or a direct route that returns the actual content. Redirect entries ensure old links aren’t broken. (In future implementation, the redirect middleware will perform `http.Redirect` to `new_path` if `old_path` matches.)

### Example Middleware Composition

The order and combination of middleware can be seen in the tenant router setup. For example, Adept adds the alias and requestinfo middleware in order:

```go
r.Use(routing.Middleware(t))    // Alias rewrite middleware (for friendly URLs)
r.Use(requestinfo.Enrich)       // Request info middleware (attach UA/IP data)
```

This means for every request on this tenant, `routing.Middleware` runs first and potentially rewrites the path, then `requestinfo.Enrich` runs and augments context. Only then do the route handlers execute.

If we wanted to secure an admin route, we could add an ACL middleware in a similar way. For instance:

```go
r.With(acl.RequireRole("admin")).Get("/admin/dashboard", adminDashHandler)
```

This composes the `RequireRole("admin")` guard with the GET handler. The effect is that a request to `/admin/dashboard` will first go through alias and requestinfo (as global for tenant), then hit the ACL guard which checks the user’s role; if not an admin, it will stop and return 401/403, otherwise it calls `adminDashHandler`. This demonstrates how multiple middleware layers stack to enforce security and other policies.

These examples illustrate common patterns: defining routes, using aliases/redirects for flexible URLs, and layering middleware for functionality and security. When developing new routes or modifying existing ones, following these patterns ensures consistency across the Adept codebase. Always test the sequence – e.g., if you add a new middleware, verify it runs at the correct point relative to aliasing and handlers. The provided examples and referenced code lines should serve as a reference when implementing similar functionality.&#x20;
