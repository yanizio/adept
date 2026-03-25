// internal/platform/config/loader.go
//
// Configuration loader and hot-reloader with Vault support.
//
// Context
// -------
// `Load()` builds one immutable `Config` struct from three layers (highest
// precedence last):
//
//  1. Optional `.env` files at known locations under the project root.
//  2. `conf/global.yaml`.
//  3. Environment variables prefixed `ADEPT_`, where `__` maps to “.”
//     (e.g., `ADEPT_HTTP__LISTEN_ADDR → http.listen_addr`).
//
// **Vault integration** — any string value that begins with the prefix
// `vault:` is treated as a Vault URI of the form
// `vault:<secret-path>#<key>` and is resolved through `internal/vault.Client`
// before unmarshalling, so callers stay oblivious.
//
// Instrumentation
// ---------------
//   - DEBUG spans — root discovery, YAML read, env overlay, Vault resolve.
//   - ERROR spans — YAML parse, env overlay, Vault fetch, unmarshal, validation.
//   - INFO  span  — final “config loaded” with key highlights.
//   - Logs use the global *sugared* logger (`zap.S()`), so early boot issues
//     surface even before the file logger is installed.
//
// Notes
// -----
//   - Oxford commas, two spaces after sentence periods.
//   - The singleton Vault client fails fast; the binary will refuse to start
//     if Vault cannot be reached.
package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/joho/godotenv"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	koanf "github.com/knadh/koanf/v2"

	adepvault "github.com/yanizio/adept/internal/platform/data/vault"
	"go.uber.org/zap"
)

var current atomic.Pointer[Config]

/*────────────────── singleton Vault client & bootstrap ─────────────────────*/

var vaultCli *adepvault.Client // nil means init failed

func ensureVault(ctx context.Context) error {
	if vaultCli != nil {
		return nil
	}

	cli, err := adepvault.New(ctx, zap.S().Debugf)
	if err != nil {
		return err
	}
	vaultCli = cli
	return nil
}

/*──────────────────────────── root discovery ───────────────────────────────*/

// rootDir resolves ADEPT_ROOT or climbs directories until conf/global.yaml is
// found.  Falls back to executable heuristic for production layout.
func rootDir() string {
	if r := os.Getenv("ADEPT_ROOT"); r != "" {
		return r
	}

	wd, _ := os.Getwd()
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "conf", "global.yaml")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir { // reached filesystem root
			break
		}
		dir = parent
	}

	exe, _ := os.Executable()
	if filepath.Base(filepath.Dir(exe)) == "bin" {
		return filepath.Dir(filepath.Dir(exe))
	}
	return wd
}

/*─────────────────────────────── loader ───────────────────────────────────*/

// Load reads .env, YAML, env overrides, resolves Vault URIs, validates, and
// caches Config.  It is safe for concurrent use.
func Load() (*Config, error) {
	ctx := context.Background()
	root := rootDir()

	// Load dotenv values before Vault init so Vault env vars can come from .env.
	loadDotEnv(root)
	zap.S().Debugw("config root resolved", "root", root)

	// Fail fast if Vault is unreachable.
	if err := ensureVault(ctx); err != nil {
		zap.S().Errorw("vault init failed", "err", err)
		return nil, err
	}

	k := koanf.New(".")

	yamlPath := filepath.Join(root, "conf", "global.yaml")
	if err := k.Load(file.Provider(yamlPath), yaml.Parser()); err != nil {
		zap.S().Errorw("config yaml load failed", "file", yamlPath, "err", err)
		return nil, err
	}
	zap.S().Debugw("config yaml loaded", "file", yamlPath)

	// Env overrides: ADEPT_HTTP__LISTEN_ADDR → http.listen_addr
	if err := k.Load(env.Provider("ADEPT_", ".", func(s string) string {
		return strings.ToLower(strings.ReplaceAll(s, "__", "."))
	}), nil); err != nil {
		zap.S().Errorw("config env overlay failed", "err", err)
		return nil, err
	}

	// Resolve Vault URIs in-place.
	if err := resolveVaultURIs(ctx, k); err != nil {
		zap.S().Errorw("config vault resolve failed", "err", err)
		return nil, err
	}

	var cfg Config
	if err := k.Unmarshal("", &cfg); err != nil {
		zap.S().Errorw("config unmarshal failed", "err", err)
		return nil, err
	}

	cfg.Paths.Root = root
	if err := validateStruct(&cfg); err != nil {
		zap.S().Errorw("config validation failed", "err", err)
		return nil, err
	}

	current.Store(&cfg)
	zap.S().Infow("config loaded",
		"listen_addr", cfg.HTTP.ListenAddr,
		"force_https", cfg.HTTP.ForceHTTPS,
		"root", cfg.Paths.Root,
	)
	return &cfg, nil
}

/*──────────────────────────── helpers ─────────────────────────────────────*/

func Get() *Config  { return current.Load() }
func Reload() error { _, err := Load(); return err }

/*──────────────────── Vault URI resolver ───────────────────────────────────*/

func loadDotEnv(root string) {
	candidates := []string{
		filepath.Join(root, ".env"),
		filepath.Join(root, "conf", ".env"),
		filepath.Join(root, "Readme", ".env"),
	}

	for _, p := range candidates {
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() {
			continue
		}
		if err := godotenv.Load(p); err != nil {
			zap.S().Warnw("dotenv load failed", "file", p, "err", err)
			continue
		}
		zap.S().Debugw("dotenv loaded", "file", p)
	}
}

func resolveVaultURIs(ctx context.Context, k *koanf.Koanf) error {
	const prefix = "vault:"

	keys := k.Keys() // snapshot to avoid concurrent mutation
	for _, key := range keys {
		val, ok := k.Get(key).(string)
		if !ok || !strings.HasPrefix(val, prefix) {
			continue
		}

		body := strings.TrimPrefix(val, prefix)
		parts := strings.SplitN(body, "#", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid vault URI %q (want vault:path#key)", val)
		}
		secretPath, field := parts[0], parts[1]

		plain, err := vaultCli.GetKV(ctx, secretPath, field, 10*time.Minute)
		if err != nil {
			return err
		}
		k.Set(key, plain)
		zap.S().Debugw("vault uri resolved",
			"key", key, "path", secretPath, "field", field)
	}
	return nil
}
