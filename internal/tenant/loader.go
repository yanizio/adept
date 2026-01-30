// internal/tenant/loader.go
//
// host → Tenant loader (Vault-aware).
//
// Performs five blocking steps per cold-load and finally runs per-tenant
// Component initialisers.

package tenant

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/yanizio/adept/internal/component"
	"github.com/yanizio/adept/internal/database"
	"github.com/yanizio/adept/internal/tenant/meta"
	"github.com/yanizio/adept/internal/theme"
	"github.com/yanizio/adept/internal/vault"
)

//
// loader
//

// loadSite executes the slow-path load in five well-defined steps, then
// invokes Init hooks for every registered Component.
func loadSite(
	ctx context.Context,
	global *sqlx.DB,
	host string,
	vcli *vault.Client,
) (*Tenant, error) {

	// 1. resolve alias and fetch site row
	lookupHost := resolveLookupHost(host)
	rec, err := meta.ByHost(ctx, global, lookupHost)
	if err != nil {
		return nil, ErrNotFound
	}

	// 2. key-value config
	cfg, err := meta.ConfigBySite(ctx, global, rec.ID)
	if err != nil {
		return nil, err
	}

	// 2a. API credentials (vault-aware).
	apiCreds, err := loadAPICreds(ctx, cfg, vcli)
	if err != nil {
		return nil, err
	}

	// 3. resolve password and build DSN
	key := sanitizeHost(host)
	pw, err := vcli.GetKV(
		ctx,
		fmt.Sprintf("secret/adept/tenant/%s/db", key),
		"password",
		10*time.Minute,
	)
	if err != nil {
		return nil, err
	}
	dsn := buildTenantDSN(key, pw)

	// 4. tenant DB pool
	opts := database.Options{
		MaxOpenConns:    5,
		MaxIdleConns:    2,
		ConnMaxLifetime: 30 * time.Minute,
		Retries:         2,
		RetryBackoff:    500 * time.Millisecond,
	}
	db, err := database.OpenProvider(ctx, func() string { return dsn }, opts)
	if err != nil {
		return nil, err
	}

	// 5. theme parsing
	enabledComponents := []string{"core"} // TODO: pull from ACL table
	mgr := theme.Manager{BaseDir: "themes"}
	th, err := mgr.Load(rec.Theme, enabledComponents)
	if err != nil {
		return nil, err
	}

	// Assemble Tenant
	ten := &Tenant{
		Meta:     *rec,
		Config:   cfg,
		API:      apiCreds,
		DB:       db,
		Theme:    th,
		Renderer: th.Renderer,
		Vault:    vcli, // expose Vault to Components
	}

	// Run per-tenant Init hooks (if implemented).
	for _, c := range component.All() {
		if initc, ok := c.(component.Initializer); ok {
			if err := initc.Init(ten); err != nil {
				return nil, err
			}
		}
	}

	return ten, nil
}
