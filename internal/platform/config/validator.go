// internal/platform/config/validator.go
//
// Thin wrapper around go-playground/validator.
//
// Context
// -------
// `internal/platform/config/loader.go` calls `validateStruct` immediately after it
// unmarshals the merged Koanf tree into a `Config` instance.  Any tag
// mismatch or validation error aborts startup, ensuring the binary never
// runs with partial, malformed, or missing configuration.
//
// The only built-in rule we rely on right now is `required`, attached to
// fields such as `Database.Engine` and the various password fields.
// Additional custom rules—e.g., “DSN must include host or socket” or
// tenant-name pattern checks—can be registered here as the configuration
// surface grows.
//
// Notes
// -----
//   • Oxford commas, two spaces after periods.
//   • Section dividers use the simple comment style requested.

package config

import (
	"fmt"

	"github.com/go-playground/validator/v10"
)

//
// validator instance (package-level singleton)
//

var v = validator.New()

//
// public API
//

// validateStruct returns the first validation error, or nil on success.
func validateStruct(c *Config) error {
	if err := v.Struct(c); err != nil {
		return err
	}
	return validateDatabase(&c.Database)
}

func validateDatabase(d *Database) error {
	if d == nil {
		return nil
	}

	if err := validateConnTarget("global", d.Global.Host, d.Global.SocketDir); err != nil {
		return err
	}
	if err := validateConnTarget("tenant", d.Tenant.Host, d.Tenant.SocketDir); err != nil {
		return err
	}
	return nil
}

func validateConnTarget(label, host, socketDir string) error {
	if host == "" && socketDir == "" {
		return fmt.Errorf("database.%s: host or socket_dir must be set", label)
	}
	if host != "" && socketDir != "" {
		return fmt.Errorf("database.%s: set host or socket_dir, not both", label)
	}
	return nil
}
