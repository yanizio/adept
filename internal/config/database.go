// internal/config/database.go
//
// Database DSN builders.
//
// Notes
// -----
// • Uses driver-compatible DSNs for PostgreSQL/Cockroach and MariaDB.
// • Supports TCP and Unix-socket connections.
// • Oxford commas, two spaces after periods.

package config

import (
	"fmt"
	"net/url"
	"strings"

	mysql "github.com/go-sql-driver/mysql"
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
	eng := strings.ToLower(strings.TrimSpace(engine))
	if eng != "postgres" && eng != "cockroach" && eng != "mariadb" && eng != "mysql" {
		return "", fmt.Errorf("database: unsupported engine %q", engine)
	}

	if eng == "mariadb" || eng == "mysql" {
		return buildMySQLDSN(user, password, dbname, spec), nil
	}

	sslmode := spec.SSLMode
	if sslmode == "" {
		sslmode = "disable"
		if eng == "cockroach" {
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
		if eng == "cockroach" {
			port = 26257
		} else {
			port = 5432
		}
	}

	u.Host = fmt.Sprintf("%s:%d", host, port)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func buildMySQLDSN(user, password, dbname string, spec connSpec) string {
	host := spec.Host
	if host == "" {
		host = "127.0.0.1"
	}

	port := spec.Port
	if port == 0 {
		port = 3306
	}

	netProto := "tcp"
	addr := fmt.Sprintf("%s:%d", host, port)
	if spec.SocketDir != "" {
		netProto = "unix"
		addr = spec.SocketDir
	}

	params := map[string]string{
		"parseTime": "true",
		"charset":   "utf8mb4",
	}
	if spec.SSLMode != "" {
		switch spec.SSLMode {
		case "disable":
			params["tls"] = "false"
		case "require", "verify-ca", "verify-full":
			params["tls"] = "true"
		}
	}

	// Start from driver defaults so auth plugin flags stay compatible with
	// MySQL/MariaDB servers (notably AllowNativePasswords=true).
	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = password
	cfg.Net = netProto
	cfg.Addr = addr
	cfg.DBName = dbname
	cfg.Params = params
	return cfg.FormatDSN()
}
