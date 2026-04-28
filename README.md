# ops-gateway

A tiny, single-binary HTTP service for out-of-band management of an [OpenClaw](https://github.com/openclaw/openclaw) gateway. Designed to run alongside the gateway as a systemd service behind a Cloudflare Tunnel with Zero Trust access.

## What it does

- **`GET /`** — Web UI with live gateway status, health history bar, and a restart button
- **`GET /health`** — No auth. Returns `{"ok":true}` if ops-gateway itself is alive
- **`GET /status`** — Authenticated. Returns current gateway status + 1-hour health history
- **`POST /restart`** — Authenticated. Restarts the gateway via `systemctl --user restart openclaw-gateway`

## Authentication

Two mechanisms (either works):

1. **Cloudflare Access (Zero Trust)** — email-based OTP, session lasts 24h. Ideal for phone access.
2. **Bearer token** — `Authorization: Bearer <token>` header. For automation/scripts.

## Setup

```bash
# Build
go build -o ops-gateway .

# Configure
export OPS_TOKEN="$(openssl rand -hex 32)"
export OPS_PORT="18790"  # default

# Run
./ops-gateway
```

### Systemd service

```ini
[Unit]
Description=Ops Gateway

[Service]
Type=simple
ExecStart=/path/to/ops-gateway
EnvironmentFile=/path/to/env
Restart=always

[Install]
WantedBy=default.target
```

### Cloudflare Tunnel + Access

1. Create a tunnel: `cloudflared tunnel create ops`
2. Route DNS: create a CNAME record pointing to `<tunnel-id>.cfargotunnel.com`
3. Create an Access application for the hostname
4. Add an Access policy (e.g., email allowlist)

The tunnel config:

```yaml
tunnel: <tunnel-id>
credentials-file: /path/to/credentials.json

ingress:
  - hostname: ops.example.com
    service: http://127.0.0.1:18790
  - service: http_status:404
```

## Health Watchdog

The included `watchdog.sh` is a companion script designed to run as a systemd timer (every 2 minutes):

- Checks gateway health via HTTP and systemd
- Auto-restarts after 3 consecutive failures
- Sends Telegram alerts on failure/recovery (bypasses OpenClaw entirely)

```ini
[Timer]
OnBootSec=1min
OnUnitActiveSec=2min
```

## Architecture

```
Phone/Browser
    │
    ▼ (HTTPS)
Cloudflare Access (Zero Trust)
    │
    ▼ (authenticated)
Cloudflare Tunnel
    │
    ▼ (localhost)
ops-gateway (:18790)
    │
    ▼ (systemctl)
openclaw-gateway
```

Zero dependencies beyond Go stdlib. ~200 lines.

## License

MIT
