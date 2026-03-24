// components/auth/auth.go
//
// Adept authentication component.
//
// Responsibilities
// ----------------
//   • Serve GET /login and POST /login routes.
//   • Render the login form via widget “auth/login”.
//   • Validate submissions using the form subsystem.
//   • Authenticate users (stubbed here) and start a session.
//
// Notes
// -----
// • Oxford commas, two spaces after periods.
// • Corresponding template file: components/auth/templates/login.html.
//
//------------------------------------------------------------------------------

package auth

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	// Side-effect import registers the “auth/login” widget at init().
	_ "github.com/yanizio/adept/components/auth/widgets"

	"github.com/yanizio/adept/internal/component"
	"github.com/yanizio/adept/internal/form"
	platformauth "github.com/yanizio/adept/internal/platform/auth"
	"github.com/yanizio/adept/internal/tenant"
	"github.com/yanizio/adept/internal/view"
)

// Compile-time assertion: *Component implements component.Component.
var _ component.Component = (*Component)(nil)

// template keys (no “.html” extension).
const tplLogin = "login"

// Component encapsulates authentication routes.
type Component struct{}

/*──────────────── component.Component interface ─────────────────────────*/

// Name returns the canonical component key.
func (c *Component) Name() string { return "auth" }

// Migrations returns nil – auth currently has no DB schema.
func (c *Component) Migrations() []string { return nil }

// Init satisfies component.Initializer; no per-tenant boot work needed.
func (c *Component) Init(component.TenantInfo) error { return nil }

// Routes builds and returns the router mounted at “/”.
func (c *Component) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/login", c.handleLoginGET)
	r.Post("/login", c.handleLoginPOST)
	return r
}

// Register the component during program init.
func init() { component.Register(&Component{}) }

/*──────────────────────────── Route handlers ─────────────────────────────*/

// handleLoginGET renders the blank login page.
func (c *Component) handleLoginGET(w http.ResponseWriter, r *http.Request) {
	vctx := tenant.NewContext(r)
	if err := view.Render(vctx, w, "auth", tplLogin, nil, view.CacheSkip); err != nil {
		zap.L().Error("login page render", zap.Error(err))
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
}

// handleLoginPOST validates the form, then logs the user in.
func (c *Component) handleLoginPOST(w http.ResponseWriter, r *http.Request) {
	vctx := tenant.NewContext(r)

	data, err := form.HandleSubmit("auth/login", r)
	if err != nil {
		if form.IsValidationError(err) {
			// Re-render form with field errors and preserved input.
			_ = view.Render(vctx, w, "auth", tplLogin, map[string]any{
				"FormErrors":  err,
				"FormPrefill": r.PostForm,
			}, view.CacheSkip)
			return
		}
		zap.L().Error("login form processing", zap.Error(err))
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	email := data["email"].(string)
	pass := data["password"].(string)
	if !checkCredentials(email, pass) {
		_ = view.Render(vctx, w, "auth", tplLogin, map[string]any{
			"FormErrors": []form.ErrorField{{
				Name:    "password",
				Message: "Incorrect email or password.",
			}},
			"FormPrefill": r.PostForm,
		}, view.CacheSkip)
		return
	}

	platformauth.LoginUser(w, r, email)
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

/*────────────────────── Stub credential checker ─────────────────────────*/

// checkCredentials is a placeholder.  Replace with real auth logic.
func checkCredentials(_, _ string) bool { return false }
