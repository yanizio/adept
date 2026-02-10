// internal/config/database.go
//
// Database DSN builders.
//
// Notes
// -----
// • Uses URL-style DSNs compatible with pgx/libpq.
// • Supports TCP and Unix-socket connections.
// • Oxford commas, two spaces after periods.

package config

import (
	"fmt"
	"net/url"
)

// GlobalDSN builds the DSN for the global database.
func (d Database) GlobalDSN() (string, error) {
	return buildDSN(d.Engine, d.Global.User, d.Global.Password, d.Global.Name, connSpec{
		Host:      d.Global.Host,
		Port:      d.Global.Port,
		SocketDir: d.Global.SocketDir,
		SSLMode:   d.Global.SSLMode,
	})
}

// TenantDSN builds the DSN for a tenant database keyed by tenantKey.
func (d Database) TenantDSN(tenantKey, password string) (string, error) {
	return buildDSN(d.Engine, tenantKey, password, tenantKey, connSpec{
		Host:      d.Tenant.Host,
		Port:      d.Tenant.Port,
		SocketDir: d.Tenant.SocketDir,
		SSLMode:   d.Tenant.SSLMode,
	})
}

type connSpec struct {
	Host      string
	Port      int
	SocketDir string
	SSLMode   string
}

func buildDSN(engine, user, password, dbname string, spec connSpec) (string, error) {
	if engine != "postgres" && engine != "cockroach" {
		return "", fmt.Errorf("database: unsupported engine %q", engine)
	}

	sslmode := spec.SSLMode
	if sslmode == "" {
		sslmode = "disable"
		if engine == "cockroach" {
			sslmode = "require"
		}
	}

	u := &url.URL{Scheme: "postgres"}
	u.User = url.UserPassword(user, password)
	u.Path = "/" + dbname

	q := url.Values{}
	q.Set("sslmode", sslmode)

	if spec.SocketDir != "" {
		// For Unix sockets, omit host and pass socket dir via query parameter.
		q.Set("host", spec.SocketDir)
		u.RawQuery = q.Encode()
		return u.String(), nil
	}

	host := spec.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := spec.Port
	if port == 0 {
		if engine == "cockroach" {
			port = 26257
		} else {
			port = 5432
		}
	}

	u.Host = fmt.Sprintf("%s:%d", host, port)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
