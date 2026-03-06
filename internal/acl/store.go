// internal/acl/store.go
//
// Small query helpers for Role‑Based Access Control.
//
// Context
// -------
// The Adept ACL model lives entirely inside the tenant database:
//
//	role        (id PK, name, enabled)
//	role_acl    (role_id, component, action, permitted)
//	user_role   (user_id, role_id)
//
// Components and middleware need fast answers to two questions:
//  1. Which *role names* does user X have?        → `UserRoles()`
//  2. Is role R permitted for component/action?   → `RoleAllowed()`
//
// These helpers accept a *sql.DB scoped to the tenant* and perform simple
// parameterised queries.  They are thin; callers may wrap the results in their
// own per‑request cache.
//
// Notes
// -----
// • Oxford commas, two spaces after periods.
// • Max line length 100 columns.
package acl

import (
	"context"
	"database/sql"

	"github.com/jmoiron/sqlx"
)

// UserRoles returns the role *names* bound to userID.  Disabled roles are
// filtered out.
func UserRoles(ctx context.Context, db *sqlx.DB, userID int64) ([]string, error) {
	const q = `SELECT r.name
                 FROM user_role ur
                 JOIN role r ON r.id = ur.role_id
                WHERE ur.user_id = ? AND r.enabled = TRUE`

	rows, err := db.QueryContext(ctx, db.Rebind(q), userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	roles := make([]string, 0, 4)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		roles = append(roles, name)
	}
	return roles, rows.Err()
}

// RoleAllowed reports whether *any* of the candidate roles is permitted for the
// given component + action.  It executes one query using IN (? … ?).
//
// Empty roles slice returns false, nil.
func RoleAllowed(ctx context.Context, db *sqlx.DB, roles []string, component, action string) (bool, error) {
	if len(roles) == 0 {
		return false, nil
	}

	q := `SELECT 1
            FROM role_acl ra
            JOIN role r ON r.id = ra.role_id
           WHERE r.name IN (?)
             AND ra.component = ?
             AND ra.action   = ?
             AND ra.permitted = TRUE
           LIMIT 1` // early exit once we find a hit

	q, args, err := sqlx.In(q, roles, component, action)
	if err != nil {
		return false, err
	}
	q = db.Rebind(q)

	var dummy int
	err = db.QueryRowContext(ctx, q, args...).Scan(&dummy)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
