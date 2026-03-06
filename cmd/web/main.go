// cmd/web/main.go
//
// Adept – HTTP entry point.
//
// Responsibilities
// ----------------
//   1. Parse configuration and bootstrap logging / Vault / DB.
//   2. Maintain an LRU cache of live *tenant.Tenant aggregates.
//   3. Load all YAML-defined forms at startup so form widgets work.
//   4. Expose Prometheus metrics at /metrics.
//   5. Route every incoming host to its per-tenant chi.Router.
//   6. Enforce optional HTTPS, always set security headers.
//   7. Start an http.Server with sane production timeouts.
//
// Notes
// -----
// • Security headers are added via middleware.Security.
// • Connection timeouts come from internal/server.New.
// • Oxford commas, two spaces after periods.

package main

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	// Side-effect imports: components self-register in init().
	_ "github.com/yanizio/adept/components/auth"    // auth routes + widgets
	_ "github.com/yanizio/adept/components/example" // sample component

	"github.com/yanizio/adept/internal/config"
	"github.com/yanizio/adept/internal/database"
	"github.com/yanizio/adept/internal/form"
	"github.com/yanizio/adept/internal/logger"
	"github.com/yanizio/adept/internal/middleware"
	"github.com/yanizio/adept/internal/server"
	"github.com/yanizio/adept/internal/tenant"
	"github.com/yanizio/adept/internal/vault"
)

// runningInTTY returns true when stdout is a character device (dev mode).
func runningInTTY() bool {
	if fi, err := os.Stdout.Stat(); err == nil {
		return fi.Mode()&os.ModeCharDevice != 0
	}
	return false
}

func main() {
	/*──────────────────────── Bootstrap phase ─────────────────────────────*/

	// 0. Early dev logger ensures boot errors are visible in TTY.
	zap.ReplaceGlobals(zap.Must(zap.NewDevelopment()))

	// 1. Load configuration (YAML → struct, with Vault URI resolution).
	cfg, err := config.Load()
	if err != nil {
		zap.L().Fatal("config load failed", zap.Error(err))
	}

	// 2. Structured JSON logger (rotates daily, colorizes in TTY).
	logOut, err := logger.New(cfg.Paths.Root, runningInTTY())
	if err != nil {
		zap.L().Fatal("logger init failed", zap.Error(err))
	}

	// 3. Vault client (token-file auth preferred, AppRole fallback).
	vaultCli, err := vault.New(context.Background(), logOut.Debugf)
	if err != nil {
		logOut.Fatalw("vault init failed", zap.Error(err))
	}

	// 4. Global DB pool (holds cross-tenant + internal tables).
	if err := database.SetEngine(cfg.Database.Engine); err != nil {
		logOut.Fatalw("database engine config failed", zap.Error(err))
	}

	dsn := func() string {
		c := config.Get()
		if c == nil {
			return ""
		}
		out, err := c.Database.GlobalDSN()
		if err != nil {
			logOut.Fatalw("global DSN build failed", zap.Error(err))
		}
		return out
	}
	globalDB, err := database.OpenProvider(context.Background(), dsn, database.Options{})
	if err != nil {
		logOut.Fatalw("global DB connect failed", zap.Error(err))
	}
	defer globalDB.Close()

	// 5. Load *all* YAML-defined forms now so widgets can render later.
	//    We pass only the repo root – form.RegisterForms walks the tree.
	if err := form.RegisterForms([]string{cfg.Paths.Root}); err != nil {
		logOut.Fatalw("form load failed", zap.Error(err))
	}

	// 6. Tenant LRU cache (30-min idle-TTL, max 100 tenants in memory).
	cache := tenant.New(globalDB, 30*time.Minute, 100, logOut, vaultCli)

	/*──────────────────────── HTTP handler setup ──────────────────────────*/

	// 7. Prometheus metrics endpoint.
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	// 8. Root handler: map Host → tenant → chi.Router.
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := stripPort(r.Host)
		ten, err := cache.Get(host)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		ten.Router().ServeHTTP(w, r)
	})

	// 9. Middleware chain: Security headers always, HTTPS redirect optional.
	var handler http.Handler = root
	handler = middleware.Security(handler)
	if cfg.HTTP.ForceHTTPS {
		handler = middleware.ForceHTTPS(cache, handler)
	}
	mux.Handle("/", handler)

	// 10. Build http.Server with sane production timeouts, then listen.
	srv := server.New(cfg.HTTP.ListenAddr, mux)

	logOut.Infow("listening", "addr", cfg.HTTP.ListenAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logOut.Fatalw("http server stopped unexpectedly", zap.Error(err))
	}
}

/*────────────────────────── Utility helpers ──────────────────────────────*/

// stripPort("example.com:443") → "example.com".
func stripPort(h string) string {
	if i := strings.IndexByte(h, ':'); i != -1 {
		return h[:i]
	}
	return h
}
