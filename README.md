# wan-ip-check

A lightweight WAN IP monitor that detects public IP changes and automatically updates Cloudflare DNS records, Cloudflare Gateway DNS locations, and sends Discord notifications.

Built for networks behind dynamic ISP-assigned IPs where DNS records and Zero Trust gateway source addresses need to stay in sync.

## What It Does

On a configurable schedule (default: every 5 minutes):

1. **Checks** your current public IP via an external service
2. **Compares** it against the last known IP
3. If the IP changed:
   - **Updates Cloudflare DNS** A records (one or multiple) with the new IP
   - **Updates Cloudflare Gateway** DNS location source IPv4 address
   - **Sends a Discord notification** with old IP, new IP, and update status
   - **Saves** the new IP to persistent state
4. If unchanged, sends a **daily summary** at a configurable time confirming the IP is stable

All Cloudflare features are optional. Without Cloudflare configuration, it works as a Discord-only IP change alerter.

## Use Cases

- **Dynamic DNS** for home servers, VPNs, or self-hosted services behind a dynamic IP
- **Cloudflare Zero Trust** — keep Gateway DNS location source IPs current when your WAN IP changes
- **IP change alerting** — get notified when your ISP assigns a new IP

## Quick Start

### Docker (recommended)

```bash
# 1. Clone the repo
git clone https://github.com/AC13139/wan-ip-check.git
cd wan-ip-check

# 2. Create your .env file
cp .env.example .env
# Edit .env with your actual values (see Configuration below)

# 3. Run with Docker
docker run -d \
  --name wan-ip-check \
  --restart unless-stopped \
  --env-file .env \
  ac13139/wan-ip-check:latest
```

Or use the pre-built image from Docker Hub:

```bash
docker pull ac13139/wan-ip-check:latest
```

### Docker Compose

Customize `docker-compose.yml` for your network setup, then:

```bash
cp .env.example .env
# Edit .env with your values
docker compose up -d
```

### Build from Source

Requires Go 1.24+.

```bash
git clone https://github.com/AC13139/wan-ip-check.git
cd wan-ip-check
go build -o wan-ip-check .
```

Run directly:

```bash
export DISCORD_WEBHOOK_URL="https://discord.com/api/webhooks/..."
export DISCORD_PROXY="http://your-proxy:port"
# Set other env vars as needed (see Configuration)
./wan-ip-check
```

Or with a system cron job (set `CRON_SCHEDULE` inside the app, or run the binary on your own schedule):

```bash
# The app has its own built-in scheduler — just run it as a service
./wan-ip-check
```

## Configuration

All configuration is via environment variables. Create a `.env` file from the template:

```bash
cp .env.example .env
```

### Required

| Variable | Description |
|----------|-------------|
| `DISCORD_WEBHOOK_URL` | Discord webhook URL for notifications |
| `DISCORD_PROXY` | HTTP proxy URL for Discord API access (required in regions where Discord is blocked) |

### Optional

| Variable | Default | Description |
|----------|---------|-------------|
| `IP_CHECK_SERVICE` | `https://icanhazip.com` | Service URL that returns your public IP as plain text |
| `CRON_SCHEDULE` | `*/5 * * * *` | Cron expression for check interval |
| `DAILY_SUMMARY_TIME` | `2000` | Time (HHMM, 24h) to send daily "no change" summary |
| `TIMEZONE` | `Local` | Timezone for cron and daily summary (e.g., `Asia/Shanghai`) |

### Cloudflare (optional)

Leave these unset to run in Discord-only mode. If you set `CF_API_TOKEN`, you must also provide the related group variables.

| Variable | Description |
|----------|-------------|
| `CF_API_TOKEN` | Cloudflare API token with DNS edit + Gateway edit permissions |
| `CF_ZONE_ID` | Zone ID (found in Cloudflare dashboard > domain > Overview) |
| `CF_DNS_RECORD_IDS` | Comma-separated DNS record IDs to update (e.g., `id1,id2`) |
| `CF_ACCOUNT_ID` | Account ID (for Zero Trust Gateway) |
| `CF_LOCATION_ID` | Gateway DNS location ID to update |

**How to find these IDs:**

- **Zone ID**: Cloudflare dashboard > select your domain > Overview > right sidebar
- **DNS Record IDs**: Use the Cloudflare API:
  ```bash
  curl -s "https://api.cloudflare.com/client/v4/zones/YOUR_ZONE_ID/dns_records" \
    -H "Authorization: Bearer YOUR_API_TOKEN" | jq '.result[] | {id, name, type, content}'
  ```
- **Account ID**: Cloudflare dashboard > account home > right sidebar
- **Location ID**: Use the Cloudflare API:
  ```bash
  curl -s "https://api.cloudflare.com/client/v4/accounts/YOUR_ACCOUNT_ID/gateway/locations" \
    -H "Authorization: Bearer YOUR_API_TOKEN" | jq '.result[] | {id, name}'
  ```

**Cloudflare API Token permissions needed:**
- Zone > DNS > Edit
- Account > Zero Trust > Edit (for Gateway locations)

### Policy-Based Routing (advanced)

For Docker containers with multiple network interfaces where IP checking and Discord notifications must route through different gateways.

| Variable | Default | Description |
|----------|---------|-------------|
| `NOTIFICATION_INTERFACE` | _(unset)_ | Network interface name that triggers PBR setup |
| `PBR_GATEWAY` | `192.168.10.1` | Gateway IP for the notification interface |
| `PBR_DEV` | `eth1` | Device for the PBR routing table |
| `PBR_TABLE` | `10` | Routing table number |

> PBR requires `cap_add: NET_ADMIN` in Docker.

## How It Works

```
                    ┌─────────────────┐
                    │  Cron Scheduler  │
                    │  (robfig/cron)   │
                    └────────┬────────┘
                             │ every N minutes
                             v
                    ┌─────────────────┐
                    │  Fetch Public IP │──── icanhazip.com
                    └────────┬────────┘     (direct)
                             │
                     ┌───────┴───────┐
                     │  IP Changed?  │
                     └───┬───────┬───┘
                    Yes  │       │  No
                         v       v
              ┌──────────────┐  ┌──────────────┐
              │ 1. CF DNS    │  │ Daily summary │
              │    PATCH     │  │ (once/day at  │
              │ 2. CF Gateway│  │  configured   │
              │    PATCH     │  │  time)        │
              │ 3. Discord   │  └──────┬───────┘
              │    notify    │         │
              │ 4. Save IP   │         v
              └──────────────┘  ┌──────────────┐
                                │ Discord      │
                                │ "No Change"  │
                                └──────────────┘
```

- Cloudflare API calls go **direct** (no proxy)
- Discord webhook calls go **through the configured proxy**
- If any Cloudflare or Discord update fails, the IP is **not saved** — the next run will retry
- The Discord notification includes Cloudflare update status (success/fail counts)

## Docker Hub

Pre-built images are available at [`ac13139/wan-ip-check`](https://hub.docker.com/r/ac13139/wan-ip-check).

| Tag | Description |
|-----|-------------|
| `latest` | Latest build from `main` branch |
| `X.Y.Z` | Specific version (e.g., `2.0.0`) |
| `X.Y` | Latest patch for a minor version |

## Project Structure

```
wan-ip-check/
├── main.go           # Entry point, HTTP clients, PBR, cron scheduler
├── config.go         # Environment variable loading and validation
├── checker.go        # IP fetch, compare, daily summary logic
├── cloudflare.go     # Cloudflare DNS + Gateway API integration
├── discord.go        # Discord webhook notifications
├── Dockerfile        # Multi-stage build (Go builder -> Alpine runtime)
├── docker-compose.yml
├── .env.example      # Configuration template
└── .github/workflows/
    └── docker-publish.yml  # CI/CD pipeline
```

## License

MIT
