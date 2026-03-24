// internal/tenant/apikeys.go
//
// Extract per-tenant API credentials from site_config.

package tenant

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yanizio/adept/internal/platform/data/vault"
)

const apiConfigPrefix = "api."

// loadAPICreds collects provider keys from tenant config.
func loadAPICreds(ctx context.Context, cfg map[string]string, vcli *vault.Client) (map[string]string, error) {
	creds := make(map[string]string)
	for key, val := range cfg {
		if !strings.HasPrefix(key, apiConfigPrefix) {
			continue
		}
		provider, field := splitProviderKey(strings.TrimPrefix(key, apiConfigPrefix))
		if provider == "" || (field != "" && field != "key") {
			continue
		}
		resolved, err := resolveVault(ctx, vcli, val)
		if err != nil {
			return nil, err
		}
		if resolved != "" {
			creds[provider] = resolved
		}
	}
	return creds, nil
}

func splitProviderKey(raw string) (string, string) {
	parts := strings.Split(raw, ".")
	if len(parts) == 0 {
		return "", ""
	}
	provider := strings.TrimSpace(parts[0])
	field := ""
	if len(parts) > 1 {
		field = strings.TrimSpace(parts[1])
	}
	return provider, field
}

func resolveVault(ctx context.Context, vcli *vault.Client, val string) (string, error) {
	val = strings.TrimSpace(val)
	if !strings.HasPrefix(val, "vault:") {
		return val, nil
	}
	trimmed := strings.TrimPrefix(val, "vault:")
	path, field, ok := strings.Cut(trimmed, "#")
	if !ok || path == "" || field == "" {
		return "", fmt.Errorf("invalid vault uri %q", val)
	}
	secret, err := vcli.GetKV(ctx, path, field, 10*time.Minute)
	if err != nil {
		return "", err
	}
	return secret, nil
}
