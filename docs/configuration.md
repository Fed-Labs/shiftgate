# Configuration reference

Every configuration surface the shipped binaries read: JSON keys per binary,
environment variables, and the validation each enforces. Precedence is
flags > environment > JSON file (`--config`) > built-in defaults. This page
is the inventory; [installation](installation.md) and
[deployment](deployment.md) show the recipes.

**File permissions:** any file passed to `--config` must be mode `0600` and
owned by the running user (the loader refuses anything readable by group or
other — installers write `0600` for exactly this reason).

**Durations:** in JSON they are raw nanosecond integers (Go `time.Duration`
encoding — `"check_interval": 21600000000000` is 6 hours). In environment
variables they are Go duration strings (`30s`, `5m`, `24h`).

## Agent (`shift-agent --config /etc/shift/agent.json`)

### JSON keys

| Key | Default | Notes |
|---|---|---|
| `version` | `1` | schema version; any other value is rejected |
| `state_dir` | `/var/lib/shift` | must be an absolute path |
| `listen` | `unix:///run/shift/agent.sock` | `unix://` or `tcp://`; a TCP listener needs `insecure_development` or full TLS |
| `socket_group` | `shift` | group the Unix socket is handed to |
| `remote_listen` | *(empty)* | e.g. `tcp://0.0.0.0:8443`; requires the `tls` block (mutual TLS) unless `insecure_development` |
| `tls.certificate_file` / `tls.private_key_file` / `tls.client_ca_file` | *(empty)* | all three required with any TCP listener |
| `tls.peer_ca_file` / `tls.server_name` | *(empty)* | outbound peer verification |
| `insecure_development` | `false` | loopback evaluation only — never production |
| `chunk_size_bytes` | `4194304` | 64 KiB–64 MiB |
| `max_concurrent_migrations` | `2` | 1–32 |
| `shutdown_timeout` | `20000000000` (20 s, ns) | graceful-shutdown budget |
| `log_level` | `info` | `debug`/`info`/`warn`/`error`; unknown values fall back to `info` |
| `cgroup_root` | `/sys/fs/cgroup/shift` | absolute, cleaned, never `/` |
| `object_store.*` | see below | checkpoint mirroring |
| `control_plane.*` | see below | dashboard presence reporting |
| `updates.*` | see below | agent self-update |
| `fencing.provider` | `disabled` | `disabled`/`self`/`command` |
| `fencing.command` / `fencing.status_command` | *(empty)* | required for `provider: command` |
| `fencing.timeout` | `30s` (ns in JSON) | ≤ 5 minutes |
| `relay.enabled` / `relay.endpoint` / `relay.ca_file` / `relay.server_name` | `false` / empty | `endpoint` + `ca_file` required when enabled |
| `tracing.otlp_endpoint` / `tracing.service_name` | *(empty)* | OTLP/HTTP JSON export; URL validated when set |
| `entitlement.live_migration` | `false` | self-asserted entitlement for detached agents (the e2e harness sets it) |

### Environment variables

| Variable | Maps to |
|---|---|
| `SHIFT_STATE_DIR` | `state_dir` |
| `SHIFT_AGENT_LISTEN` | `listen` |
| `SHIFT_SOCKET_GROUP` | `socket_group` |
| `SHIFT_LOG_LEVEL` | `log_level` |
| `SHIFT_CGROUP_ROOT` | `cgroup_root` |
| `SHIFT_TRACING_SERVICE_NAME` | `tracing.service_name` (agent) |
| `SHIFT_OTLP_ENDPOINT` | `tracing.otlp_endpoint` (agent and control) |
| `SHIFT_OBJECTSTORE_ENABLED` / `SHIFT_OBJECTSTORE_BACKEND` / `SHIFT_OBJECTSTORE_LOCAL_ROOT` / `SHIFT_OBJECTSTORE_STATE_DIR` | `object_store.*` |
| `SHIFT_S3_ENDPOINT` / `SHIFT_S3_REGION` / `SHIFT_S3_BUCKET` / `SHIFT_S3_ACCESS_KEY_ID` / `SHIFT_S3_SECRET_ACCESS_KEY` / `SHIFT_S3_SESSION_TOKEN` / `SHIFT_S3_PREFIX` / `SHIFT_S3_FORCE_PATH_STYLE` | `object_store.*` (S3 backend) |
| `SHIFT_CONTROL_URL` / `SHIFT_CONTROL_ORGANIZATION_ID` / `SHIFT_CONTROL_MACHINE_ID` / `SHIFT_CONTROL_MACHINE_NAME` / `SHIFT_CONTROL_AGENT_URL` / `SHIFT_CONTROL_API_KEY` / `SHIFT_CONTROL_REPORT_INTERVAL` | `control_plane.*` |
| `SHIFT_UPDATE_ENABLED` / `SHIFT_UPDATE_CHANNEL` / `SHIFT_UPDATE_POLICY` / `SHIFT_UPDATE_FEED_URL` / `SHIFT_UPDATE_FEED_FILE` / `SHIFT_UPDATE_TRUSTED_KEYS_FILE` / `SHIFT_UPDATE_SIGNATURE_THRESHOLD` / `SHIFT_UPDATE_CHECK_INTERVAL` / `SHIFT_UPDATE_EXECUTABLE` | `updates.*` |
| `SHIFT_AGENT` | legacy alias of `SHIFT_AGENT_ENDPOINT` (CLI) |

`SHIFT_CONTROL_URL` unset means the platform's control plane. Values are
all-or-nothing: `url`, `organization_id`, `machine_id`, and `api_key` are set
together or not at all.

### Object store (`object_store`)

| Key | Default | Notes |
|---|---|---|
| `enabled` | `false` | |
| `backend` | `local` | `local` / `s3` / `control-plane` |
| `local_root` | `<state_dir>/cloud-objects` | absolute when enabled |
| `endpoint` / `region` / `bucket` | *(empty)* | required for `s3` |
| `access_key_id` / `secret_access_key` / `session_token` / `prefix` | *(empty)* | never store these in a committed config |
| `state_dir` | `<state_dir>/objectstore-state` | absolute for `s3`/`control-plane` |
| `force_path_style` | `false` | |

Mirrors are not a recovery path — restore reads local state only
([limitations](limitations.md)).

### Updates (`updates`)

| Key | Default | Notes |
|---|---|---|
| `enabled` | `false` | requires feed + trusted keys |
| `channel` | `stable` | `stable`/`beta`/`nightly` |
| `policy` | `mandatory` | `manual`/`mandatory`/`automatic` |
| `feed_url` / `feed_file` | *(empty)* | `https` only; mutually exclusive |
| `trusted_keys` / `trusted_keys_file` | *(empty)* | one required when enabled |
| `signature_threshold` | `1` | |
| `check_interval` | `21600000000000` (6 h) | ≥ 15 m, ≤ 7 d |
| `feed_timeout` / `download_timeout` | `30s` / `10m` | |
| `keep_backups` | `3` | ≤ 10 |
| `executable_path` / `staging_dir` / `backup_dir` / `state_file` | derived under `<state_dir>/updates` | must be absolute; staging ≠ backup |

## Control plane (`shift-control --config /etc/shift/control.json`)

The systemd unit reads `/etc/shift/control.env` (see
`deployments/systemd/control.env.example`) and `/etc/shift/control.json`
(0600, owned by the unit's `shift` user).

### JSON keys

| Key | Default | Notes |
|---|---|---|
| `version` | `1` | schema version |
| `listen` | `127.0.0.1:8090` | required, non-empty |
| `database_url` | *(empty)* | required (PostgreSQL) |
| `token_pepper` / `password_pepper` | *(empty)* | required; ≥ 32 characters each |
| `log_level` | `info` | unknown values fall back to `info` |
| `session_ttl` | `720h` | must be ≥ refresh TTL |
| `access_token_ttl` | `15m` | 1 m–24 h |
| `refresh_token_ttl` | `720h` | 1 h–1 y |
| `short_refresh_token_ttl` | `24h` | 1 m–refresh TTL |
| `rate_limit_per_minute` | `120` | 10–10000 |
| `request_timeout` | `30s` | 0–5 m |
| `compute_trading_enabled` | `false` | |
| `compute_reservation_ttl` | `15m` | 1 m–24 h |
| `retention_sweep_interval` | `1h` | `0` disables; ≤ 7 d |
| `allowed_origins` | `nil` | browser origins (CORS) |
| `local_agent_socket_root` | `/run/shift` | absolute, never `/` |
| `agent_tls.*` | *(empty)* | client certificate/key and peer CA for remote agent dispatch |
| `oidc.issuer` / `client_id` / `client_secret` | *(empty)* | all-or-none; `https` issuer |
| `github.client_id` / `client_secret` | *(empty)* | "Sign in / Sign up with GitHub"; all-or-none — without them the GitHub auth endpoints answer `503 GITHUB_NOT_CONFIGURED` |
| `github.allowed_organizations` | `nil` | optional GitHub org-logins allowlist: when set, only members of those orgs may authenticate (checked at the OAuth callback, failing closed); empty admits any GitHub user |
| `stripe_secret_key` / `stripe_webhook_key` / `stripe_prices` / `checkout_coupons` | *(empty)* | billing (optional) |
| `storage.*` | disabled | hosted checkpoint storage |
| `relay_signing_key` / `relay_endpoint` | *(empty)* | ≥ 32 chars when set |
| `trusted_proxies` | `nil` | CIDRs/IPs whose `X-Forwarded-For` is honored |
| `metrics_token` | *(empty)* | required for `/metrics`; ≥ 16 chars |
| `migration_worker_concurrency` | `4` | 1–64 |
| `allow_remote_agent_dispatch` | `false` | `true` allows managed dispatch via the machine's public peer URL when `agent_tls` client certificate/key/peer CA are configured — a security-relevant widening |
| `tracing.otlp_endpoint` / `tracing.service_name` | *(empty)* | |

### Environment variables

| Variable | Maps to |
|---|---|
| `SHIFT_DATABASE_URL` | `database_url` (also `shift-relay`) |
| `SHIFT_TOKEN_PEPPER` / `SHIFT_PASSWORD_PEPPER` | the two peppers |
| `SHIFT_CONTROL_LISTEN` | `listen` (when unset and the platform supplies `PORT`, binds `0.0.0.0:$PORT` instead of the loopback default) |
| `SHIFT_ALLOWED_ORIGINS` | `allowed_origins` (comma-separated) |
| `SHIFT_SHORT_REFRESH_TOKEN_TTL` | `short_refresh_token_ttl` |
| `SHIFT_LOCAL_AGENT_SOCKET_ROOT` | `local_agent_socket_root` |
| `SHIFT_COMPUTE_TRADING_ENABLED` | `compute_trading_enabled` |
| `SHIFT_CHECKOUT_COUPONS` | `checkout_coupons` |
| `SHIFT_TRUSTED_PROXIES` | `trusted_proxies` |
| `SHIFT_METRICS_TOKEN` | `metrics_token` |
| `SHIFT_MIGRATION_WORKER_CONCURRENCY` | `migration_worker_concurrency` |
| `SHIFT_ALLOW_REMOTE_AGENT_DISPATCH` | `allow_remote_agent_dispatch` |
| `SHIFT_AGENT_TLS_CERT` / `SHIFT_AGENT_TLS_KEY` / `SHIFT_AGENT_TLS_PEER_CA` / `SHIFT_AGENT_TLS_SERVER_NAME` | `agent_tls.*` for remote agent dispatch (cert/key/CA accept PEM material or file paths) |

| `SHIFT_OTLP_ENDPOINT` / `SHIFT_OTLP_SERVICE_NAME` | `tracing.*` (control; note: the agent's service-name variable is `SHIFT_TRACING_SERVICE_NAME`) |
| `SHIFT_RELAY_SIGNING_KEY` / `SHIFT_RELAY_ENDPOINT` | `relay_signing_key` / `relay_endpoint` |
| `SHIFT_OIDC_ISSUER` / `SHIFT_OIDC_CLIENT_ID` / `SHIFT_OIDC_CLIENT_SECRET` | `oidc.*` |
| `SHIFT_GITHUB_CLIENT_ID` / `SHIFT_GITHUB_CLIENT_SECRET` | `github.client_id` / `github.client_secret` ("Sign in / Sign up with GitHub"; the OAuth app's callback URL must be `<control>/v1/auth/github/callback`) |
| `SHIFT_GITHUB_ALLOWED_ORGANIZATIONS` | `github.allowed_organizations` (comma-separated GitHub org logins; when set, only members of those organizations may authenticate — the check runs at the OAuth callback and fails closed to `github_org_denied`, so a user whose org membership is private is denied too) |
| `SHIFT_STRIPE_PRICE_<PLAN>[_ANNUAL]` / `SHIFT_STRIPE_ADDON_*` | `stripe_prices` |
| `SHIFT_STORAGE_ENABLED` / `SHIFT_STORAGE_ENDPOINT` / `SHIFT_STORAGE_PUBLIC_ENDPOINT` / `SHIFT_STORAGE_REGION` / `SHIFT_STORAGE_BUCKET` / `SHIFT_STORAGE_ACCESS_KEY_ID` / `SHIFT_STORAGE_SECRET_ACCESS_KEY` / `SHIFT_STORAGE_CREDENTIAL_TTL` / `SHIFT_STORAGE_RECONCILE_INTERVAL` | `storage.*` (TTL 15 m–7 d; reconcile ≥ 30 s) |

## Relay (`shift-relay`)

| Flag | Env | Required |
|---|---|---|
| `--listen` | `SHIFT_RELAY_LISTEN` | default `:9443` (or `:$PORT` when the platform supplies `PORT`) |
| `--cert` / `--key` | `SHIFT_RELAY_CERT` / `SHIFT_RELAY_KEY` | yes (PEM material or file paths) |
| `--client-ca` | `SHIFT_RELAY_CLIENT_CA` | yes (machine client CA PEM or file path) |
| `--audience` | `SHIFT_RELAY_AUDIENCE` | yes |
| `--database-url` | `SHIFT_DATABASE_URL` | yes (durable leg store) |
| `--signing-key` | `SHIFT_RELAY_SIGNING_KEY` | yes (32+ bytes) |

See [relay.md](relay.md) for the full prerequisites.

## CLI (`shiftgate`) environment

| Variable | Purpose |
|---|---|
| `SHIFT_AGENT_ENDPOINT` (or legacy `SHIFT_AGENT`) | agent endpoint override (default `unix:///run/shift/agent.sock`) |
| `SHIFT_CONTROL_URL` | control plane base URL |
| `SHIFT_CONNECTIONS_FILE` | connection profiles path |
| `SHIFT_NO_HINTS` | suppress non-error hints |
| `XDG_CONFIG_HOME` | session store root (`~/.config/shift/cli-session.json`, 0600) |

## Installer environment

`SHIFT_PREFIX`, `SHIFT_ALLOW_USER_PREFIX`, `SHIFT_INSTALL_SERVICE`,
`SHIFT_RELEASE_URL`, `SHIFT_INSTALL_SHA256` (pin for the release installer)
— see [installation.md](installation.md).
