// internal/session/session.go
//
// Adept – Session stub.
//
// Context
//   Authentication requires persisting a “logged-in” flag between requests.
//   This scaffold sets or clears a signed cookie named “adept_session” that
//   stores the user’s email address in plaintext.  It is **NOT** production-
//   ready but unblocks compilation and basic manual testing.
//
//   Replace these helpers with a full session store backed by Redis, JWT, or
//   your preferred strategy.  All callers (e.g., components/auth) rely only on
//   this tiny API, so swapping the implementation is painless.
//
// Style
//   Two-space sentence spacing, Oxford comma, terse inline notes.
//
//------------------------------------------------------------------------------

package auth

import (
	"net/http"
	"time"
)

const (
	cookieName = "adept_session"
	// A robust implementation would AES-GCM-encrypt + HMAC-sign the payload.
)

// LoginUser sets a session cookie containing the user’s email.
//
// Callers typically invoke this after credential verification succeeds.
func LoginUser(w http.ResponseWriter, r *http.Request, email string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    email, // TODO: encrypt + sign
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil, // only send over HTTPS
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(14 * 24 * time.Hour),
	})
}

// LogoutUser clears the session cookie.
func LogoutUser(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	})
}

// CurrentEmail returns the email stored in the session, if any.
//
// ok == false when the cookie is missing or empty.
func CurrentEmail(r *http.Request) (email string, ok bool) {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return "", false
	}
	return c.Value, true
}
