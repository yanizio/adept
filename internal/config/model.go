// internal/config/model.go
//
// Typed configuration model for Adept.
//
// Context
// -------
// These structs define the shape of the configuration tree that
// `internal/config/loader.go` builds from three overlay layers:
//
//   • optional `.env`                         – dotenv values,
//   • `conf/global.yaml`                      – primary static file,
//   • `ADEPT_`-prefixed environment overrides – highest precedence.
//
// Any value whose string begins with the prefix `vault:` is resolved
// through the Vault client *before* unmarshalling, so the model never
// stores Vault URIs—only plain strings.
//
// Validation happens immediately after unmarshal; the app fails fast if
// required fields are missing.
//
// Notes
// -----
//   • Struct tags use `koanf:"…"`, not `yaml:"…"`—Koanf ignores `yaml` tags
//     unless configured otherwise.
//   • The `Paths` block is filled at runtime; YAML must not try to set it.
//   • Oxford commas, two spaces after periods.  No em-dash.

package config

//
// HTTP section
//

// HTTP holds web-server tunables.
type HTTP struct {
	ListenAddr string `koanf:"listen_addr" validate:"required,hostname_port"`
	ForceHTTPS bool   `koanf:"force_https"`
}

//
// Database section
//

// Database holds the selected engine and connection details.
//
//   - *Engine* selects postgres, cockroach, or mariadb.
//   - *Global* includes the global database name and credentials.
//   - *Tenant* includes connection details plus Vault path for per-tenant
//     passwords.
//   - *LocalhostAlias* lets dev instances map the host string "localhost"
//     to a unique schema/user key (default "devlocal") so they do not
//     collide with production names.
type Database struct {
	Engine         string       `koanf:"engine"          validate:"required,oneof=postgres cockroach mariadb mysql"`
	Global         DBConnection `koanf:"global"          validate:"required"`
	Tenant         TenantDB     `koanf:"tenant"          validate:"required"`
	LocalhostAlias string       `koanf:"localhost_alias" validate:"omitempty"`
}

// DBConnection holds connection details for a single database.
type DBConnection struct {
	Host      string `koanf:"host"       validate:"omitempty"`
	Port      int    `koanf:"port"       validate:"omitempty,min=1,max=65535"`
	SocketDir string `koanf:"socket_dir" validate:"omitempty"`
	Name      string `koanf:"name"       validate:"required"`
	User      string `koanf:"user"       validate:"required"`
	Password  string `koanf:"password"   validate:"required"`
	SSLMode   string `koanf:"sslmode"    validate:"omitempty,oneof=disable require verify-ca verify-full"`
}

// TenantDB holds tenant connection details and Vault lookup hints.
type TenantDB struct {
	Host         string `koanf:"host"          validate:"omitempty"`
	Port         int    `koanf:"port"          validate:"omitempty,min=1,max=65535"`
	SocketDir    string `koanf:"socket_dir"    validate:"omitempty"`
	SSLMode      string `koanf:"sslmode"       validate:"omitempty,oneof=disable require verify-ca verify-full"`
	PasswordPath string `koanf:"password_path" validate:"required"`
	PasswordKey  string `koanf:"password_key"  validate:"required"`
}

//
// Paths section (runtime only)
//

// Paths is resolved at runtime—never set in YAML or env.  The loader
// discovers `Root` (repo root or ADEPT_ROOT override) so later code can
// build absolute file paths.
type Paths struct {
	Root string // ADEPT_ROOT or discovered parent
}

//
// Root aggregate
//

// Config is the immutable aggregate returned by Load() and cached in an
// atomic.Pointer for lock-free reads throughout the app lifetime.
type Config struct {
	HTTP     HTTP     `koanf:"http"`
	Database Database `koanf:"database"`
	Paths    Paths    `koanf:"-"` // not loaded from config files
}
