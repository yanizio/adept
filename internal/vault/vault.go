// internal/vault/vault.go
//
// Vault client wrapper for Adept.
//
// Context
// -------
//   - Provides a concurrency-safe singleton around the HashiCorp Vault Go SDK.
//   - Adds auth bootstrapping for Vault Agent token-file and AppRole flows.
//   - Adds startup health checks, token maintenance, and per-key caching.
//
// Public workflow
// ---------------
//  1. cli, err := vault.New(ctx, log.Printf)       // during boot.
//  2. pw,  err := cli.GetKV(ctx, path, key, ttl)   // anywhere in the app.
//
// Build tags: none.
package vault

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	vault "github.com/hashicorp/vault/api"
)

const (
	defaultDialTimeout    = 5 * time.Second
	defaultRequestTimeout = 10 * time.Second
)

type authMode string

const (
	authModeTokenEnv  authMode = "token_env"
	authModeTokenFile authMode = "token_file"
	authModeAppRole   authMode = "approle"
)

//
// SECTION 1.  Public facade
//

// Client is safe for concurrent use.  Create once at startup and inject it via
// your DI container.  Zero value is invalid.
type Client struct {
	api   *vault.Client
	logFn func(string, ...any)

	authMode  authMode
	token     string
	tokenFile string
	roleID    string
	secretID  string
	renewable bool
	leaseDur  time.Duration
	authMu    sync.Mutex
	cacheMu   sync.RWMutex
	cache     map[string]cached // canonical path#key -> value + expiry.
}

type cached struct {
	val string
	exp time.Time
}

// New constructs a Vault client, performs startup auth, validates health, and
// starts background auth maintenance when needed.
//
// Environment expectations
// ------------------------
// • VAULT_ADDR       – HTTPS Vault address, e.g. https://vault.example:8200.
// • VAULT_CACERT     – optional PEM CA file path for Vault TLS verification.
// • VAULT_TOKEN      – direct token for local/dev use.
// • VAULT_TOKEN_FILE – preferred token source from Vault Agent sink file.
// • VAULT_ROLE_ID + VAULT_SECRET_ID – AppRole fallback when token file is unset.
func New(ctx context.Context, logFn func(string, ...any)) (*Client, error) {
	if logFn == nil {
		logFn = func(string, ...any) {}
	}

	env, err := loadAndValidateEnv()
	if err != nil {
		return nil, err
	}

	cfg, err := buildConfig(env)
	if err != nil {
		return nil, err
	}

	apiCli, err := vault.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("vault api client init failed: %w", err)
	}

	c := &Client{
		api:       apiCli,
		logFn:     logFn,
		authMode:  env.mode,
		token:     env.token,
		tokenFile: env.tokenFile,
		roleID:    env.roleID,
		secretID:  env.secretID,
		cache:     make(map[string]cached),
	}

	if err := withInitialRetry(ctx, func() error { return c.initialAuth(ctx) }); err != nil {
		return nil, err
	}
	if err := withInitialRetry(ctx, func() error { return c.checkHealth(ctx) }); err != nil {
		return nil, err
	}

	if c.authMode == authModeAppRole && c.renewable {
		go c.appRoleRenewLoop(ctx)
	}

	return c, nil
}

// GetKV fetches a single key from a KV-v2 secret.  If ttl > 0 the result is
// cached for that duration.  Subsequent callers within the TTL receive the
// cached copy.
func (c *Client) GetKV(ctx context.Context, secretPath, key string, ttl time.Duration) (string, error) {
	if secretPath == "" || key == "" {
		return "", errors.New("secret path and key must be non-empty")
	}

	canonical := secretPath + "#" + key

	if ttl > 0 {
		c.cacheMu.RLock()
		if cv, ok := c.cache[canonical]; ok && time.Now().Before(cv.exp) {
			c.cacheMu.RUnlock()
			return cv.val, nil
		}
		c.cacheMu.RUnlock()
	}

	sval, err := c.getKV(ctx, secretPath, key)
	if err != nil {
		if c.authMode == authModeTokenFile && isUnauthorized(err) {
			if rerr := c.reloadTokenFromFile(); rerr == nil {
				sval, err = c.getKV(ctx, secretPath, key)
			}
		}
		if err != nil {
			return "", fmt.Errorf("vault get %s: %w", secretPath, err)
		}
	}

	if ttl > 0 {
		c.cacheMu.Lock()
		c.cache[canonical] = cached{val: sval, exp: time.Now().Add(ttl)}
		c.cacheMu.Unlock()
	}

	return sval, nil
}

func (c *Client) getKV(ctx context.Context, secretPath, key string) (string, error) {
	mount, rel := splitMount(secretPath)
	sec, err := c.api.KVv2(mount).Get(ctx, rel)
	if err != nil {
		return "", err
	}

	raw, ok := sec.Data[key]
	if !ok {
		return "", fmt.Errorf("key %q not found in secret %q", key, secretPath)
	}

	sval, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("value at %s#%s is not a string", secretPath, key)
	}
	return sval, nil
}

//
// SECTION 2.  Startup and auth maintenance
//

type vaultEnv struct {
	addr      string
	caCert    string
	token     string
	tokenFile string
	roleID    string
	secretID  string
	mode      authMode
}

func loadAndValidateEnv() (vaultEnv, error) {
	env := vaultEnv{
		addr:      strings.TrimSpace(os.Getenv("VAULT_ADDR")),
		caCert:    strings.TrimSpace(os.Getenv("VAULT_CACERT")),
		token:     strings.TrimSpace(os.Getenv("VAULT_TOKEN")),
		tokenFile: strings.TrimSpace(os.Getenv("VAULT_TOKEN_FILE")),
		roleID:    strings.TrimSpace(os.Getenv("VAULT_ROLE_ID")),
		secretID:  strings.TrimSpace(os.Getenv("VAULT_SECRET_ID")),
	}

	if env.addr == "" {
		return vaultEnv{}, fmt.Errorf("missing VAULT_ADDR: set VAULT_ADDR to your Vault URL, e.g. https://vault-dev.example.internal:8200")
	}
	if !strings.HasPrefix(strings.ToLower(env.addr), "https://") {
		return vaultEnv{}, fmt.Errorf("invalid VAULT_ADDR %q: only https:// Vault addresses are supported", env.addr)
	}

	if env.caCert != "" {
		if _, err := os.Stat(env.caCert); err != nil {
			return vaultEnv{}, fmt.Errorf("invalid VAULT_CACERT %q: %w", env.caCert, err)
		}
	}

	if env.token != "" {
		env.mode = authModeTokenEnv
		return env, nil
	}

	if env.tokenFile != "" {
		env.mode = authModeTokenFile
		return env, nil
	}

	if env.roleID == "" || env.secretID == "" {
		return vaultEnv{}, fmt.Errorf("missing Vault auth env vars: set VAULT_TOKEN (dev), VAULT_TOKEN_FILE, or set both VAULT_ROLE_ID and VAULT_SECRET_ID")
	}
	env.mode = authModeAppRole
	return env, nil
}

func buildConfig(env vaultEnv) (*vault.Config, error) {
	cfg := vault.DefaultConfig()
	cfg.Address = env.addr
	cfg.Timeout = defaultRequestTimeout

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: defaultDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   defaultDialTimeout,
		ResponseHeaderTimeout: defaultRequestTimeout,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	cfg.HttpClient = &http.Client{
		Transport: transport,
		Timeout:   defaultRequestTimeout,
	}

	if env.caCert != "" {
		if err := cfg.ConfigureTLS(&vault.TLSConfig{CACert: env.caCert}); err != nil {
			return nil, fmt.Errorf("vault tls config failed: %w", err)
		}
	}

	return cfg, nil
}

func (c *Client) initialAuth(ctx context.Context) error {
	switch c.authMode {
	case authModeTokenEnv:
		c.api.SetToken(c.token)
		return nil
	case authModeTokenFile:
		return c.reloadTokenFromFile()
	case authModeAppRole:
		sec, err := c.loginAppRole(ctx)
		if err != nil {
			return err
		}
		if sec != nil && sec.Auth != nil {
			c.renewable = sec.Auth.Renewable
			c.leaseDur = time.Duration(sec.Auth.LeaseDuration) * time.Second
		}
		return nil
	default:
		return errors.New("unsupported vault auth mode")
	}
}

func (c *Client) reloadTokenFromFile() error {
	c.authMu.Lock()
	defer c.authMu.Unlock()

	b, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return fmt.Errorf("read VAULT_TOKEN_FILE %q failed: %w", c.tokenFile, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return fmt.Errorf("VAULT_TOKEN_FILE %q is empty", c.tokenFile)
	}
	c.api.SetToken(tok)
	return nil
}

func (c *Client) loginAppRole(ctx context.Context) (*vault.Secret, error) {
	c.authMu.Lock()
	defer c.authMu.Unlock()

	sec, err := c.api.Logical().WriteWithContext(ctx, "auth/approle/login", map[string]any{
		"role_id":   c.roleID,
		"secret_id": c.secretID,
	})
	if err != nil {
		return nil, fmt.Errorf("vault AppRole login failed: %w", err)
	}
	if sec == nil || sec.Auth == nil || sec.Auth.ClientToken == "" {
		return nil, errors.New("vault AppRole login returned no client token")
	}
	c.api.SetToken(sec.Auth.ClientToken)
	return sec, nil
}

func (c *Client) appRoleRenewLoop(ctx context.Context) {
	for {
		wait := renewalWait(c.leaseDur)
		if !backoff(ctx, wait) {
			return
		}

		reqCtx, cancel := context.WithTimeout(ctx, defaultRequestTimeout)
		sec, err := c.api.Auth().Token().RenewSelfWithContext(reqCtx, 0)
		cancel()

		if err != nil {
			c.logFn("vault: token renewal failed, attempting AppRole re-login: %v", err)
			reqCtx, cancel = context.WithTimeout(ctx, defaultRequestTimeout)
			sec, err = c.loginAppRole(reqCtx)
			cancel()
			if err != nil {
				c.logFn("vault: AppRole re-login failed: %v", err)
				if !backoff(ctx, 5*time.Second) {
					return
				}
				continue
			}
		}

		if sec != nil && sec.Auth != nil {
			c.leaseDur = time.Duration(sec.Auth.LeaseDuration) * time.Second
		}
	}
}

func (c *Client) checkHealth(ctx context.Context) error {
	req := c.api.NewRequest(http.MethodGet, "/v1/sys/health")
	resp, err := c.api.RawRequestWithContext(ctx, req)
	if err != nil {
		return fmt.Errorf("vault health request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vault health check failed: status=%d message=%q", resp.StatusCode, msg)
	}
	return nil
}

func withInitialRetry(ctx context.Context, fn func() error) error {
	var lastErr error
	backoffDur := 250 * time.Millisecond
	for i := 0; i < 4; i++ {
		if err := fn(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if i == 3 {
			break
		}
		if !backoff(ctx, backoffDur) {
			return ctx.Err()
		}
		backoffDur *= 2
	}
	return lastErr
}

func renewalWait(leaseDur time.Duration) time.Duration {
	if leaseDur <= 0 {
		return time.Minute
	}
	wait := leaseDur * 2 / 3
	if wait < 30*time.Second {
		return 30 * time.Second
	}
	return wait
}

func isUnauthorized(err error) bool {
	var respErr *vault.ResponseError
	return errors.As(err, &respErr) && respErr.StatusCode == http.StatusUnauthorized
}

//
// SECTION 3.  Helpers
//

func splitMount(p string) (mount, rel string) {
	if p == "" {
		return "", ""
	}
	parts := strings.SplitN(p, "/", 2)
	mount = parts[0]
	if len(parts) == 2 {
		rel = parts[1]
	}
	return
}

func backoff(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
