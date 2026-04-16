# wan-ip-check

A self-hosted WAN IP monitor with a Web UI for managing what happens when your public IP changes. Configure **Actions** — Cloudflare DNS updates, Gateway location syncs, Discord alerts, custom webhooks — and monitor multiple gateways from one dashboard.

## Why This Tool?

**"But Cloudflare already has DDNS tools..."**

Yes — tools like [oznu/cloudflare-ddns](https://github.com/oznu/cloudflare-ddns) update your DNS records when your IP changes. They do one thing well. But if your needs go beyond a single DNS update, you'll end up running multiple containers, scripts, and cron jobs stitched together:

| Need | cloudflare-ddns | wan-ip-check |
|------|----------------|--------------|
| Update DNS A records | Yes | Yes (Action) |
| Update Cloudflare Gateway source IP | No | Yes (Action) |
| Notify via Discord/webhook | No | Yes (Action) |
| Monitor multiple gateways | No | Yes (WAN Interfaces) |
| Web dashboard for status | No | Yes |
| IP change history with logs | No | Yes |
| Custom webhook to any service | No | Yes (Action) |
| Configure via UI (not just env vars) | No | Yes |

**wan-ip-check is for users who:**
- Run a home server with a dynamic ISP IP and need more than DNS updates
- Use Cloudflare Zero Trust and need Gateway DNS location source IPs to stay current
- Have multiple WAN gateways (ISP + VPN) that need independent monitoring
- Want one tool, one container, one dashboard — not a stack of single-purpose scripts

## Architecture

```
┌─────────────────────────────────────────────────────┐
│                   wan-ip-check                      │
│                                                     │
│  WAN Interfaces ──→ IP Detection ──→ Actions        │
│                                                     │
│  ┌──────────────┐    ┌───────────────────┐         │
│  │ Home Gateway  │    │ Cloudflare DNS    │         │
│  │ VPN Gateway   │    │ Cloudflare Gateway│         │
│  │ ...           │    │ Discord           │         │
│  └──────────────┘    │ Custom Webhook    │         │
│                       └───────────────────┘         │
│                                                     │
│  Web UI (:7373)  ·  REST API  ·  SQLite DB          │
└─────────────────────────────────────────────────────┘
```

- **WAN Interfaces** detect your WAN IP using configurable probe services (primary + fallback)
- **Actions** execute when an IP changes (or as a daily summary)
- **WAN Interface→Action links** let different gateways trigger different actions
- Everything configurable via Web UI — no YAML or config files to manage

## Quick Start

### Docker (recommended)

```bash
docker run -d \
  --name wan-ip-check \
  --restart unless-stopped \
  -p 7373:7373 \
  -v wan-ip-data:/app/data \
  -e DISCORD_PROXY=http://your-proxy:port \
  ac13139/wan-ip-check:latest
```

Open `http://your-server:7373` to configure WAN interfaces and actions via the Web UI.

### Docker Compose

```bash
# 1. Clone and configure
git clone https://github.com/AC13139/wan-ip-check.git
cd wan-ip-check
cp .env.example .env
# Edit .env — at minimum set DISCORD_PROXY if using Discord

# 2. Run
docker compose up -d

# 3. Open the dashboard
open http://localhost:7373
```

### Build from Source

Requires Go 1.25+.

```bash
git clone https://github.com/AC13139/wan-ip-check.git
cd wan-ip-check
go build -o wan-ip-check .
./wan-ip-check
```

### Migrate from v1 / Environment Variables

If you're upgrading from v1 or prefer env-var configuration, set your variables in `.env` and start the container. On first run with an empty database, wan-ip-check **automatically migrates** your env vars into the database:

- `DISCORD_WEBHOOK_URL` → creates a Discord action
- `CF_API_TOKEN` + `CF_ZONE_ID` + `CF_DNS_RECORD_IDS` → creates a CF DNS action
- `CF_API_TOKEN` + `CF_ACCOUNT_ID` + `CF_LOCATION_ID` → creates a CF Gateway action
- `CRON_SCHEDULE`, `DAILY_SUMMARY_TIME`, `TIMEZONE` → seeded into settings

After migration, the database is the source of truth. Use the Web UI to make changes.

## Web UI

The dashboard at `http://localhost:7373` provides:

- **Dashboard** — Current IP per WAN interface, health status, recent events
- **WAN Interfaces** — Manage IP detection interfaces (probe URLs, schedule, interface binding)
- **Actions** — Configure what happens on IP change (Cloudflare, Discord, webhooks)
- **History** — Filterable event log with per-action results
- **Settings** — Daily summary time, timezone, encryption status

## Action Types

### Cloudflare DNS

Updates A records when your IP changes. Supports multiple records per action.

Configure with your API token, zone ID, and record IDs. To find record IDs, use the Cloudflare API:

```bash
curl -s "https://api.cloudflare.com/client/v4/zones/YOUR_ZONE_ID/dns_records" \
  -H "Authorization: Bearer YOUR_API_TOKEN" | jq '.result[] | {id, name, type, content}'
```

> **Planned**: The Web UI will add a record browser in a future release so you can select records without looking up IDs manually.

### Cloudflare Gateway

Updates Zero Trust Gateway DNS location source IPv4 address. Essential for Cloudflare Zero Trust users with dynamic IPs — keeps your Gateway policies working when your ISP changes your IP.

### Discord

Sends rich embed notifications via webhook. Shows old IP, new IP, WAN interface name, and a daily "no change" summary. Requires an HTTP proxy in regions where Discord is blocked (e.g., China).

### Custom Webhook

Send a POST/PUT/PATCH to any URL with a templated body. Use Go template syntax:

```json
{"ip": {{json .NewIP}}, "old": {{json .OldIP}}, "wan_interface": {{json .WANInterfaceName}}, "event": {{json .Event}}}
```

Available fields: `{{.NewIP}}`, `{{.OldIP}}`, `{{.WANInterfaceName}}`, `{{.Event}}`, `{{.Timestamp}}`. Use `{{json .Field}}` for safe JSON escaping.

## Multi-WAN-Interface Monitoring

If you have multiple WAN gateways (e.g., ISP + VPN), create a WAN Interface for each:

- **Home Gateway** → Cloudflare DNS for `home.example.com` + Discord alert
- **VPN Gateway** → Cloudflare DNS for `vpn.example.com` + CF Gateway location update

Each WAN interface checks independently on its own schedule, with its own probe URLs, and triggers only its linked actions.

## IP Detection

Each WAN interface probes external services to determine the public IP. Default strategy: **primary + fallback** — try each URL in order, return the first valid IPv4.

Default probe URLs:
1. `https://api.ipify.org`
2. `https://checkip.amazonaws.com`
3. `https://ifconfig.me/ip`

You can add, remove, or reorder URLs per WAN interface. For multi-gateway setups, use interface binding to ensure each WAN interface checks through the correct gateway.

## Security

- **Secrets encrypted at rest**: API tokens and webhook URLs are encrypted with AES-256-GCM before storing in SQLite. Encryption key is auto-generated on first run (back it up).
- **Web UI masks secrets**: Tokens shown as `•••••` — never displayed in plaintext.
- **CSRF protection**: All state-changing UI requests validated with tokens.
- **No auth by default**: The Web UI is intended for local network access or behind a reverse proxy with authentication. API auth is planned for a future release.

## Configuration Reference

### Infrastructure (always from env vars)

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `7373` | Web UI + API port |
| `DATA_DIR` | `./data` | SQLite database + encryption key directory |
| `DISCORD_PROXY` | — | HTTP proxy for Discord API access |
| `ENCRYPTION_KEY` | auto-generated | Hex-encoded 32-byte AES key (optional) |

### PBR Routing (advanced)

For Docker containers with multiple network interfaces:

| Variable | Default | Description |
|----------|---------|-------------|
| `NOTIFICATION_INTERFACE` | — | Network interface triggering PBR setup |
| `PBR_GATEWAY` | `192.168.10.1` | Gateway for notification interface |
| `PBR_DEV` | `eth1` | Device for PBR routing table |
| `PBR_TABLE` | `10` | Routing table number |

> PBR requires `cap_add: NET_ADMIN` in Docker.

## REST API

All endpoints are available at `/api/*` for programmatic access:

```bash
# Status
curl http://localhost:7373/api/status

# List WAN interfaces with state
curl http://localhost:7373/api/wan-interfaces

# Trigger manual check
curl -X POST http://localhost:7373/api/wan-interfaces/1/check

# List actions (secrets masked)
curl http://localhost:7373/api/actions

# View history
curl http://localhost:7373/api/history?limit=50
```

## Docker Hub

Pre-built images: [`ac13139/wan-ip-check`](https://hub.docker.com/r/ac13139/wan-ip-check)

| Tag | Description |
|-----|-------------|
| `latest` | Latest build from `main` |
| `X.Y.Z` | Specific version |
| `X.Y` | Latest patch for minor version |

## Project Structure

```
wan-ip-check/
├── main.go              # Entry point, HTTP clients, PBR, signal handling
├── config.go            # Boot config, env-to-DB migration
├── database.go          # SQLite schema, migrations, queries
├── wan_interface.go     # WAN interface model, IP detection (primary+fallback)
├── dispatcher.go        # Cron scheduler, WAN interface→event→action dispatch
├── action.go            # Action interface, registry, encryption helpers
├── action_cfdns.go      # Cloudflare DNS executor
├── action_cfgateway.go  # Cloudflare Gateway executor
├── action_discord.go    # Discord webhook executor
├── action_webhook.go    # Custom webhook with text/template
├── crypto.go            # AES-256-GCM encryption
├── server.go            # HTTP server, routing, middleware
├── handlers_api.go      # REST API handlers
├── handlers_ui.go       # Web UI handlers + HTMX partials
├── templates/           # Go HTML templates (embedded)
├── static/              # CSS + HTMX (embedded)
├── Dockerfile           # Multi-stage build
└── docker-compose.yml
```

## License

This project is licensed under the [GNU General Public License v3.0](LICENSE).
