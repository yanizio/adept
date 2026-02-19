// internal/acl/store_test.go
//
// Unit-tests for acl.store helpers using sqlmock.
//
// Run: go test ./internal/acl -v

package acl

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestUserRoles(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT r.name FROM user_role ur JOIN role r ON r.id = ur.role_id WHERE ur.user_id = $1 AND r.enabled = TRUE`,
	)).
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("editor").AddRow("admin"))

	got, err := UserRoles(context.Background(), db, 42)
	if err != nil {
		t.Fatalf("UserRoles error: %v", err)
	}
	if len(got) != 2 || got[0] != "editor" || got[1] != "admin" {
		t.Fatalf("unexpected result: %#v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet SQL expectations: %v", err)
	}
}

func TestRoleAllowed(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	inClause := "$1, $2" // two role names
	q := `SELECT 1 FROM role_acl ra JOIN role r ON r.id = ra.role_id WHERE r.name IN (` + inClause + `) AND ra.component = $3 AND ra.action = $4 AND ra.permitted = TRUE LIMIT 1`

	mock.ExpectQuery(regexp.QuoteMeta(q)).
		WithArgs("editor", "admin", "content", "edit").
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))

	ok, err := RoleAllowed(context.Background(), db,
		[]string{"editor", "admin"}, "content", "edit")
	if err != nil {
		t.Fatalf("RoleAllowed error: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok = true, got false")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet SQL expectations: %v", err)
	}
}
