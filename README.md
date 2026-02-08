# Adept Framework

Adept is a Go‑native **multi‑tenant** web and API platform.  It hosts many independent sites from a single binary, lazy‑loads each tenant on the first request, and evicts idle ones to keep memory usage low.  A unified *API* layer and provider‑agnostic *AI* helpers let Components call external services, such as OpenAI or SendGrid, with a single line of code.  Secrets, including database credentials, are resolved at runtime from HashiCorp Vault using automatically renewed AppRole tokens.  All documentation and code follow the Oxford comma rule, and every sentence contains two spaces after each period.

---

## Features

* **Lazy‑loaded tenants:** cold load on first hit, idle TTL plus LRU eviction.
* **Per‑tenant database pools:** five‑connection cap, DSN pulled from Vault or tenant table.
* **Vault‑resolved secrets:** `vault:` URI support in YAML or environment, with auto‑renew AppRole tokens.
* **Structured logging, metrics, tracing:** Zap JSON logs, Prometheus metrics on `/metrics`, OTLP spans.
* **Security engine:** host, IP, Geo, and User‑Agent allow or deny lists, with shadow mode metrics.
* **User‑Agent normalization:** single parser (**uasurfer**) attaches parsed UA info to the request context.
* **Unified API layer:** authentication, retry, rate‑limit, and TTL caching helpers.
* **AI helpers:** Chat and Embed wrappers that choose the provider at runtime.
* **Messaging subsystem:** provider‑agnostic email, SMS, and push notifications with queue, templating, opt‑out, and delivery logs.
* **Account and profile model:** one account may hold multiple profiles, with auto‑selection when only one exists.
* **Theme and asset manager:** bundles CSS or JS, serves pre‑compressed Brotli versions.
* **One‑directory deployment:** drop the `/inet` tree onto any server and start.

---

## Quick Start (for development)

```bash
# Clone and build
git clone https://github.com/yanizio/adept.git
cd adept
go mod tidy
go build ./cmd/web

# Seed Vault with a test secret (if not already running)
vault kv put secret/adept/global/db password='devpass'

# One‑time Vault AppRole setup
./init.sh

# Run the app with dynamic Vault credentials
./run.sh
```

Visit `http://127.0.0.1:8080/` for the placeholder page, and `http://127.0.0.1:8080/metrics` for live statistics.

---

## Directory Layout (partial)

```text
cmd/
  web/                    # HTTP entry point
internal/
  api/                    # generic client helpers and service directories (openai, …)
  ai/                     # provider‑agnostic Chat or Embed helpers
  config/                 # environment and YAML loader, Vault resolver, validation
  vault/                  # singleton Vault client with renewal loop
  database/               # sqlx helpers and migrations
  tenant/                 # meta models and lazy‑load LRU cache
  security/               # IP, UA, Geo rules and shadow mode metrics
  message/                # unified email, SMS, push queue + providers + worker
  observability/          # zap, prometheus, OTLP spans, sentry integration
components/               # first‑class business features
themes/                   # templates and assets
conf/                     # global.yaml, security.yaml, and related files
logs/                     # daily JSON logs
sql/                      # install and migration SQL
```

---

## Configuration and Environment

Most tunables live in `conf/global.yaml`.  Any key can be overridden by setting an environment variable with the `ADEPT_` prefix and replacing dots with double underscores (`__`).

| Variable or YAML key       | Example value                                       | Purpose                                |
| -------------------------- | --------------------------------------------------- | -------------------------------------- |
| `database.global_dsn`      | `postgres://adept:%s@127.0.0.1:5432/adept?sslmode=disable` | PostgreSQL DSN template, insert password |
| `database.global_password` | `vault:secret/adept/global/db#password`             | Secret password resolved through Vault |
| `http.listen_addr`         | `127.0.0.1:8080`                                    | Bind address                           |
| `http.force_https`         | `true`                                              | Send a 308 redirect for non‑HTTPS      |
| `adept_root` *(env only)*  | `/inet`                                             | One‑directory deployment root          |
| `VAULT_TOKEN` *(env)*      | dynamic                                             | Set by AppRole or other login methods  |

PostgreSQL is the current and permanent default database.  CockroachDB support is planned for a later cloud deployment mode, and will ship alongside separate schemas and driver wiring when ready.

---

## Building on Linux or macOS

```bash
make build          # runs go vet, go test, and go build
make run            # starts the server with sane defaults
```

A systemd service or launchd plist typically sets `WorkingDirectory=/inet` and sources `/usr/local/etc/adept/global.env` before launch.

---

## Contributing

1. Fork the repository and create a feature branch.
2. Run `make vet test`.  Ensure `go vet` and `golangci‑lint` pass.
3. Submit a pull request with a clear description.

We follow the comment style in `guidelines/comment-style.md`.

---

## License

BSD 3‑Clause (see `LICENSE`).
