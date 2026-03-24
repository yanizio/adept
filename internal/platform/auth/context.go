// internal/auth/context.go
//
// Minimal user-ID helper placed here so other packages (e.g. ACL middleware)
// can compile and `go vet` passes.  A full authentication system will later
// replace or extend this stub.
//
// Usage
// -----
//     // Attach user 123 to the request context (after login).
//     ctx = auth.WithUser(ctx, 123)
//
//     // Downstream code retrieves the ID.
//     id, ok := auth.UserID(ctx)   // 123, true
//
// Notes
// -----
// • Stores an int64 directly in context.  You may swap this for a richer
//   struct once session management is implemented.
// • Oxford commas, two spaces after periods.

package auth

import "context"

// userKey is unexported to avoid context-key collisions.
type userKey struct{}

// WithUser returns a new context carrying the given userID.
func WithUser(ctx context.Context, userID int64) context.Context {
	return context.WithValue(ctx, userKey{}, userID)
}

// UserID extracts the userID from ctx.  It returns (0, false) if no user is set
// or if the stored value is not an int64.
func UserID(ctx context.Context) (int64, bool) {
	v := ctx.Value(userKey{})
	id, ok := v.(int64)
	return id, ok
}
