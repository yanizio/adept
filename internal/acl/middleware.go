// internal/acl/middleware.go
//
// Chi middleware helpers that enforce RBAC.

package acl

import (
	"net/http"

	"go.uber.org/zap"

	"github.com/yanizio/adept/internal/platform/auth"
	"github.com/yanizio/adept/internal/tenant"
)

// RequireRole ensures the current user possesses ANY of the supplied roles.
func RequireRole(names ...string) func(http.Handler) http.Handler {
	if len(names) == 0 {
		panic("acl.RequireRole: at least one role name must be supplied")
	}
	allowSet := make(map[string]struct{}, len(names))
	for _, n := range names {
		allowSet[n] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			uid, ok := auth.UserID(r.Context())
			if !ok {
				http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
				return
			}

			t := tenant.FromContext(r.Context())
			if t == nil {
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				return
			}

			roles, err := UserRoles(r.Context(), t.GetDB(), uid)
			if err != nil {
				zap.L().Error("acl user roles", zap.Error(err))
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				return
			}
			for _, rname := range roles {
				if _, ok := allowSet[rname]; ok {
					next.ServeHTTP(w, r)
					return
				}
			}
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		})
	}
}

// RequirePermission verifies that user’s roles allow component/action.
func RequirePermission(component, action string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			uid, ok := auth.UserID(r.Context())
			if !ok {
				http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
				return
			}
			t := tenant.FromContext(r.Context())
			if t == nil {
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				return
			}

			roles, err := UserRoles(r.Context(), t.GetDB(), uid)
			if err != nil {
				zap.L().Error("acl user roles", zap.Error(err))
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				return
			}

			allowed, err := RoleAllowed(r.Context(), t.GetDB(), roles, component, action)
			if err != nil {
				zap.L().Error("acl role allowed", zap.Error(err))
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				return
			}
			if !allowed {
				http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
