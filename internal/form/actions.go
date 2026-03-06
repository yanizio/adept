// internal/form/actions.go
//
// Adept – Forms subsystem: post-submit actions.
//
// Context
//   A FormDef may contain default actions.  ExecuteActions dispatches to
//   runEmail, runStore, runWebhook, or runPDF (stub) after validation.  Each
//   helper queues work via Adept’s messaging subsystem so HTTP requests return
//   promptly.
//
// Style
//   Two-space sentence spacing, Oxford comma, concise inline notes.
//
//------------------------------------------------------------------------------

package form

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yanizio/adept/internal/database"
	"github.com/yanizio/adept/internal/logger"
	"github.com/yanizio/adept/internal/message"
)

// ActionCtx carries request-scoped helpers for action execution.
type ActionCtx struct{ Ctx context.Context }

// ExecuteActions performs all YAML-declared actions.  Errors are logged but not
// returned, keeping user flow uninterrupted.
func ExecuteActions(formID string, data map[string]any, actx ActionCtx) {
	fd, ok := GetFormDef(formID)
	if !ok || len(fd.Actions) == 0 {
		return
	}

	for _, ac := range fd.Actions {
		switch ac.Type {
		case "email":
			if err := runEmail(fd, ac.Params, data, actx); err != nil {
				logErr(actx, fd.ID, "email", err)
			}
		case "store":
			if err := runStore(fd, ac.Params, data, actx); err != nil {
				logErr(actx, fd.ID, "store", err)
			}
		case "webhook":
			if err := runWebhook(fd, ac.Params, data, actx); err != nil {
				logErr(actx, fd.ID, "webhook", err)
			}
		case "pdf":
			if err := runPDF(fd, ac.Params, data, actx); err != nil {
				logErr(actx, fd.ID, "pdf", err)
			}
		default:
			logWarn(actx, fd.ID, ac.Type, "unsupported action")
		}
	}
}

// -----------------------------------------------------------------------------
// Email action
// -----------------------------------------------------------------------------

func runEmail(fd *FormDef, p map[string]any, data map[string]any, actx ActionCtx) error {
	// Recipients
	var to []string
	switch v := p["to"].(type) {
	case string:
		to = []string{v}
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok {
				to = append(to, s)
			}
		}
	default:
		return fmt.Errorf("'to' parameter missing or invalid")
	}
	if len(to) == 0 {
		return fmt.Errorf("'to' parameter empty")
	}

	subject, _ := p["subject"].(string)
	if subject == "" {
		subject = fmt.Sprintf("Adept form submission: %s", fd.Title)
	}

	body, _ := json.MarshalIndent(data, "", "  ")
	msg := message.Email{
		To:      to,
		Subject: subject,
		Text:    string(body),
	}
	return message.EnqueueEmail(actx.Ctx, msg)
}

// -----------------------------------------------------------------------------
// Store action
// -----------------------------------------------------------------------------

func runStore(fd *FormDef, p map[string]any, data map[string]any, actx ActionCtx) error {
	table, _ := p["table"].(string)
	if table == "" {
		table = "form_submission"
	}

	db := database.Conn(actx.Ctx)
	if db == nil {
		return fmt.Errorf("store action: database connection unavailable")
	}
	j, err := json.Marshal(data)
	if err != nil {
		return err
	}
	q := db.Rebind(fmt.Sprintf(`INSERT INTO %s (form_id, submitted_at, data) VALUES (?,?,?)`, table))

	_, err = db.ExecContext(
		actx.Ctx,
		q,
		fd.ID,
		time.Now().UTC(),
		j,
	)
	return err
}

// -----------------------------------------------------------------------------
// Webhook action
// -----------------------------------------------------------------------------

func runWebhook(fd *FormDef, p map[string]any, data map[string]any, actx ActionCtx) error {
	url, ok := p["url"].(string)
	if !ok || url == "" {
		return fmt.Errorf("webhook action requires 'url'")
	}
	method, _ := p["method"].(string)
	if method == "" {
		method = http.MethodPost
	}

	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(actx.Ctx, method, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range p {
		if strings.HasPrefix(k, "header.") {
			req.Header.Set(strings.TrimPrefix(k, "header."), fmt.Sprint(v))
		}
	}
	return message.EnqueueWebhook(actx.Ctx, req)
}

// -----------------------------------------------------------------------------
// PDF action (stub)
// -----------------------------------------------------------------------------

func runPDF(_ *FormDef, _ map[string]any, _ map[string]any, _ ActionCtx) error {
	return fmt.Errorf("PDF action not yet implemented")
}

// -----------------------------------------------------------------------------
// Logging helpers
// -----------------------------------------------------------------------------

func logErr(actx ActionCtx, formID, action string, err error) {
	logger.FromContext(actx.Ctx).Error(
		"form action failed",
		"form", formID, "action", action, "error", err.Error(),
	)
}

func logWarn(actx ActionCtx, formID, action, msg string) {
	logger.FromContext(actx.Ctx).Warn(
		"form action warning",
		"form", formID, "action", action, "warning", msg,
	)
}
