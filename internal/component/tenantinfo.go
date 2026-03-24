// internal/component/tenantinfo.go
package component

import (
	"github.com/jmoiron/sqlx"
	"github.com/yanizio/adept/internal/platform/data/vault"
	"github.com/yanizio/adept/internal/theme"
)

// TenantInfo exposes per-tenant resources to Components during Init.
type TenantInfo interface {
	GetDB() *sqlx.DB
	GetConfig() map[string]string
	GetTheme() *theme.Theme
	GetVault() *vault.Client
}
