# wan-ip-check

A self-hosted WAN IP monitor with a Web UI that reacts to IP changes. Point any number of WAN interfaces at the public internet, and wire up **Actions** — Cloudflare DNS updates, Zero Trust Gateway location sync, Discord/webhook notifications — from a single dashboard. No YAML, no cron files, no glue scripts.

[![Docker](https://img.shields.io/docker/pulls/ac13139/wan-ip-check.svg)](https://hub.docker.com/r/ac13139/wan-ip-check)
[![License](https://img.shields.io/badge/license-GPLv3-blue.svg)](LICENSE)

## Why This Tool?

**"But Cloudflare already has DDNS tools..."**

Tools like [oznu/cloudflare-ddns](https://github.com/oznu/cloudflare-ddns) update DNS records when your IP changes — they do one thing well. Once your needs go past a single DNS update, you end up running several single-purpose containers, a couple of bash scripts, and a pile of cron entries.

| Need | cloudflare-ddns | wan-ip-check |
|------|-----------------|--------------|
| Update DNS A records | Yes | Yes (Action) |
| Update Cloudflare Zero Trust Gateway External IP | No | Yes (Action) |
| Notify via Discord / webhook | No | Yes (Action) |
| Monitor multiple WAN gateways (e.g. ISP + VPN) | No | Yes |
| Per-WAN egress routing (default / specific gateway / proxy) | No | Yes (fwmark PBR) |
| Web dashboard for status + diagnostics | No | Yes |
| Full check + action-run history with filters | No | Yes |
| Configure by UI, not env vars / YAML | No | Yes |

**wan-ip-check is for users who:**
- Run a home server on a dynamic ISP IP and need more than just DNS updates
- Use Cloudflare Zero Trust and need Gateway DNS location External IPs to stay current
- Have multiple WAN gateways (dual ISP, ISP + VPN) monitored independently
- Want one container, one dashboard, one audit log

## Features

- **Multi-WAN monitoring** — any number of WAN interfaces, each with its own schedule, probe URLs, and egress path
- **Three egress modes** per WAN (pick from a dropdown):
  - **Default route** — normal outbound
  - **Specific gateway** — fwmark-based PBR, so multiple WANs sharing a single source IP (Docker macvlan with two gateways on the same subnet) still each route through the right gateway
  - **HTTP proxy** — for environments where the outbound path goes through an internal proxy
- **Network Diagnostics panel** on the WAN form auto-detects Docker runtime, `NET_ADMIN` capability, available interfaces, and default gateway — recommends which egress mode will work
- **Four action types**:
  - **Cloudflare DNS Record** — zone dropdown, records by name (`@`, `*`, `api`, `api.example.com`), per-record proxied (orange cloud) toggle, missing records auto-created on first update
  - **Cloudflare Gateway Locations** — keeps Zero Trust DNS location External IP aligned with your WAN
  - **Discord Webhook** — rich-embed notifications, optional HTTP proxy for blocked regions
  - **HTTP Webhook** — configurable method / headers / Go-template body to any endpoint
- **Triggers on actions** — an action declares *"triggered by `<WAN>` when `<event>`"*, so one Cloudflare DNS record action is wired in one form. Linked-Actions on the WAN side stays as a bulk-link path
- **Event History** merges check outcomes (ok / changed / failed) with action-run outcomes into a single filterable timeline
- **Execute** a saved action on demand against the current observed IP; **Test** an in-progress config without saving (read-only probe for Cloudflare DNS, synthetic send for Discord/webhook)
- **Secrets encrypted at rest** via AES-256-GCM; CSRF-protected UI
- **MCP server for AI agents** — read-only tools over [Model Context Protocol](https://modelcontextprotocol.io/) (2026-07-28 spec) on a separate port; bearer-token auth; secrets never returned

## Quick Start

### Docker

```bash
docker run -d \
  --name wan-ip-check \
  --restart unless-stopped \
  --cap-add=NET_ADMIN \
  -p 7373:7373 \
  -v wan-ip-data:/app/data \
  ac13139/wan-ip-check:latest
```

Open `http://your-server:7373` and configure via the Web UI.

`--cap-add=NET_ADMIN` is only required if you plan to use **Specific gateway** egress mode. Default and proxy modes work without it.

### Docker Compose

```bash
git clone https://github.com/AC13139/wan-ip-check.git
cd wan-ip-check
cp .env.example .env   # optional — only if you want Discord proxy or custom port
docker compose up -d
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

## Web UI

`http://localhost:7373` lands on **WAN Interfaces** (the home page).

- **WAN Interfaces** — card grid; each card shows status dot, current IP, last-check time, linked-action count, and Edit / Check IP / Delete controls. `+` card opens the editor for a new WAN.
- **Actions** — card grid with Execute / Delete per card; `+` card for a new action. Execute fans the action out over every linked WAN using each WAN's current observed IP.
- **History** — merged timeline of every IP check (scheduled + manual) and every action run, filterable by WAN, event type, and status.
- **Settings** — daily summary time, timezone, encryption key path.

## Schedule

Every WAN has a simple `Check every [N] [Minutes | Hours | Days]` selector. Minutes 1–59, Hours 1–23, Days 1–31. If you had a custom cron on an older install, the form shows the raw expression and lets you edit it — otherwise the dropdown is the primary path.

## Action Types

### Cloudflare DNS Record

One API token, one zone picked from a dropdown, and any number of record names. Per record you choose **Proxied** (orange cloud) on or off. Missing records are created automatically on the first IP update; existing records are patched in place (only `content` + `proxied` — your comments, tags, and settings are preserved).

Required token permissions: **Zone:Read** + **DNS:Edit**.

Record name examples:

| Name | Resolves to |
|------|-------------|
| `@` | Zone root (e.g. `example.com`) |
| `*` | Wildcard (`*.example.com` — note: does **not** cover the apex) |
| `api` | Short name, auto-expanded to `api.example.com` |
| `api.example.com` | Kept as-is |

**Test** button is a read-only probe: verifies the token, confirms zone access, lists records, and reports which configured names already exist vs. will be created. No mutations, no history entries. Also flags name conflicts (existing CNAME blocking an A).

Legacy `record_ids` configs from earlier versions keep running; editing surfaces a migration banner. Re-save through the new form to adopt the name-based shape.

### Cloudflare Gateway Locations

Updates the **External IP** on a Cloudflare Zero Trust Gateway DNS location so your filtering policies keep matching traffic from your WAN. Required: API token, Account ID, Location ID.

### Discord Webhook

Rich embed on IP change and (optionally) as a daily summary when the IP has **not** changed. Set `DISCORD_PROXY` globally, or a per-action proxy, if Discord is unreachable from your network.

### HTTP Webhook

POST / PUT / PATCH / GET to any URL with JSON headers and a Go-template body.

```json
{"ip": {{json .NewIP}}, "old": {{json .OldIP}}, "wan": {{json .WANInterfaceName}}, "event": {{json .Event}}}
```

Available fields: `{{.NewIP}}`, `{{.OldIP}}`, `{{.WANInterfaceName}}`, `{{.Event}}`, `{{.Timestamp}}`. Wrap with `{{json .Field}}` for safe JSON escaping.

## Triggers

Each action has a **Triggers** card: *"Triggered by `<WAN>` when `<event>`"*, one row per WAN that should fire it. Events: IP changes, Daily summary (no change), or Both. Multi-WAN actions (Discord aggregate notifications) just add more rows with `+ Add trigger`.

The WAN edit form also has a **Linked Actions** section — same join table, same effect. Use it when creating a new WAN alongside several existing actions to bulk-link in one pass.

## Multi-Gateway on Single-NIC Containers

A common setup: one container with one interface, but two physical gateways on the same subnet (e.g. Docker macvlan with two ISPs). Per-WAN **Specific gateway** mode installs fwmark-based PBR rules so each WAN's probes go through its own gateway, without changing the host default route. Concurrent checks are safe.

Requires `--cap-add=NET_ADMIN`. The Network Diagnostics panel on the WAN form will detect this and recommend **Specific gateway** mode.

## IP Detection

Each WAN runs its own probe chain. Default URLs:

1. `https://api.ipify.org`
2. `https://checkip.amazonaws.com`
3. `https://ifconfig.me/ip`

Strategy is **fallback** by default (try in order until one returns a valid IPv4) or **single** (only hit the first URL). Timeout is per-WAN.

Only IPv4 today. IPv6 / AAAA is tracked in [issue #28](https://github.com/AC13139/wan-ip-check/issues/28).

## Event History

Every check outcome is recorded — even unchanged ok results — and merged with action-run records into a single timeline. Filter by WAN, event, or status. Manual "Check IP" clicks appear with `trigger=manual`; scheduled runs are `trigger=scheduled`. Auto-recycling of stale check rows is tracked in [issue #29](https://github.com/AC13139/wan-ip-check/issues/29).

## Security

- **Secrets encrypted at rest** with AES-256-GCM. Key is auto-generated in `data/encryption.key` on first run — **back it up**, or pin it with `ENCRYPTION_KEY` (32-byte hex).
- **UI masks secrets** as `•••••`; secrets never leave the server in plaintext.
- **CSRF protection** on every state-changing request.
- **No built-in auth** — intended for local networks or behind an authenticating reverse proxy. API auth is tracked as a roadmap item.

## Configuration Reference

### Infrastructure (env vars only)

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `7373` | Web UI + API port |
| `DATA_DIR` | `./data` | SQLite + encryption key directory |
| `DISCORD_PROXY` | — | Global HTTP proxy for Discord actions |
| `ENCRYPTION_KEY` | auto-generated | 32-byte hex AES key (optional pin) |
| `MCP_HTTP_PORT` | `7374` | MCP server port; set `MCP_DISABLED=1` to suppress |
| `MCP_AUTH_TOKEN` | _empty = dev mode_ | Bearer token for the MCP endpoint. Generate with `openssl rand -hex 32` |

### Upgrade / Migration

- **From v2.0.0-alpha** — schema change `sources → wan_interfaces` runs automatically on first start. Data preserved.
- **From v1 env-var config** — set `DISCORD_WEBHOOK_URL`, `CF_API_TOKEN` + `CF_ZONE_ID` + `CF_DNS_RECORD_IDS`, etc. On first run against an empty DB they are seeded into Actions. After the first start the DB is source of truth.

## REST API

All endpoints under `/api/*`:

```bash
curl http://localhost:7373/api/status
curl http://localhost:7373/api/wan-interfaces          # with state
curl -X POST http://localhost:7373/api/wan-interfaces/1/check   # manual check
curl http://localhost:7373/api/actions                  # secrets masked
curl http://localhost:7373/api/history?limit=50
curl http://localhost:7373/api/network/diagnostics     # runtime + caps + recommendation
```

## MCP Server for AI Agents

A read-only [Model Context Protocol](https://modelcontextprotocol.io/) server runs alongside the Web UI on a separate port (default `7374`). It implements the [2026-07-28 MCP spec](https://modelcontextprotocol.io/specification/2026-07-28) and exposes wan-ip-check state to any MCP-compatible client (Claude Code, custom agents, IDE plugins) — no hardcoded URLs, no manual JSON parsing.

### What's exposed

| Tool | Returns |
|---|---|
| `list_wan_interfaces` | Every WAN with current state, last IP, status, last check, last error |
| `get_wan_status` | Full WAN detail with state |
| `list_actions` | All configured actions (Discord / CF DNS / CF Gateway / Webhook) — **secrets masked** |
| `get_recent_history` | Merged check + action-run timeline for one WAN, paginated |
| `get_failed_actions` | Action runs queued for retry or abandoned after max retries |
| `get_settings` | Non-secret settings + retention TTLs |

All six tools declare `readOnlyHint=true`, `destructiveHint=false`, `idempotentHint=true`, `openWorldHint=false` so a client UI can safely render a read-only badge and never offer a "modify" button. Tool results include both `structuredContent` (modern clients) and a text fallback (legacy clients); tool errors use `isError: true`, distinct from JSON-RPC protocol errors.

### What's deliberately NOT exposed

- **Write tools** (`trigger_check`, `execute_action`) — Phase 2 (#37), gated behind a week of production soak
- **`update_*` / `delete_*` on WANs and actions** — too destructive for an unsupervised agent
- **Decrypted secrets, the DB encryption key, the data directory** — masked or unreferenced

### Enable it

Add to `.env` (or your compose environment):

```bash
MCP_HTTP_PORT=7374
MCP_AUTH_TOKEN=$(openssl rand -hex 32)
```

Then `docker compose pull && docker compose up -d`. The MCP endpoint comes up alongside the Web UI; `curl http://localhost:7374/healthz` returns `ok`.

### Connect Claude Code

**Project scope** (recommended for solo use): copy `.mcp.json.example` to `.mcp.json` and edit. `.mcp.json` is gitignored.

```bash
cp .mcp.json.example .mcp.json
# Default uses ${WAN_IP_CHECK_MCP_TOKEN} env var — export it in your shell:
export WAN_IP_CHECK_MCP_TOKEN="<your MCP_AUTH_TOKEN value>"
claude
```

Verify with `/mcp` — `wan-ip-check` should show `Connected`. Test by asking:

```
Use the list_wan_interfaces MCP tool and report the current IP for each WAN.
```

For plaintext token in the file instead of env-var, edit `.mcp.json` and replace `${WAN_IP_CHECK_MCP_TOKEN}` with the literal token.

**User scope** (available across all your projects): `claude mcp add --scope user --transport streamable-http wan-ip-check http://localhost:7374/mcp` then add the bearer token via `claude mcp edit`.

### Connect other clients

Any client that speaks MCP Streamable HTTP works. Examples:

```bash
# List tools
curl -X POST http://localhost:7374/mcp \
  -H "Authorization: Bearer $MCP_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'

# Call a tool
curl -X POST http://localhost:7374/mcp \
  -H "Authorization: Bearer $MCP_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call",
       "params":{"name":"get_recent_history","arguments":{"wan_id":1,"limit":10}}}'
```

stdio transport is also wired for clients that the prefer subprocess model — the process reads JSON-RPC from stdin and writes responses to stdout. Phase 2 will add per-token rate limiting; Phase 1 is unbounded by design until you've validated the auth and masking surfaces.

### Security

- **Bearer token enforced** by default. Missing token → `401` with `WWW-Authenticate: Bearer`. Constant-time comparison via `crypto/subtle.ConstantTimeCompare` (no timing oracle).
- **Dev mode**: if `MCP_AUTH_TOKEN` is unset the server logs a warning and serves unauthenticated. Use only for local debugging.
- **Reverse proxy recommended**: put the MCP port behind nginx/caddy to enforce network ACL + TLS. The bearer token is the second layer, not the first.
- **Secret-masking test** in `mcp_server_test.go` proves no `api_token` / `webhook_url` / `IsSecret` field ever reaches any content block — including `structuredContent`, which modern clients consume directly.

## Docker Hub

[`ac13139/wan-ip-check`](https://hub.docker.com/r/ac13139/wan-ip-check)

| Tag | Description |
|-----|-------------|
| `latest` | Current stable, tracks `main` |
| `X.Y.Z` | Pinned version (e.g. `2.0.0`) |
| `X.Y` | Latest patch of a minor line |
| `beta` | Rolling pre-release from `v2-dev` |
| `v2-dev` | Same as `beta`, branch-named alias |
| `beta-<sha>` | Immutable per-commit pre-release |

## Project Structure

```
wan-ip-check/
├── main.go                        # Entry point, signal handling, HTTP clients
├── config.go                      # Boot config, env-to-DB migration
├── database.go                    # SQLite schema, migrations, queries
├── wan_interface.go               # WAN model, IP detection, schedule helpers
├── dispatcher.go                  # Cron scheduler, PBRManager, event dispatch
├── network_diagnostics.go         # Runtime / NET_ADMIN / gateway detection
├── action.go                      # ActionExecutor interface, registry, crypto
├── action_cfdns.go                # Cloudflare DNS (new name-based flow)
├── action_cfgateway.go            # Cloudflare Zero Trust Gateway
├── action_discord.go              # Discord webhook
├── action_webhook.go              # HTTP webhook with template body
├── crypto.go                      # AES-256-GCM
├── server.go                      # HTTP server, routing, middleware
├── mcp_server.go                  # MCP server (2026-07-28 spec) registration + transports
├── mcp_tools.go                   # MCP read-tool implementations
├── mcp_auth.go                    # MCP bearer-token middleware
├── mcp_server_test.go             # MCP masking, annotations, auth tests
├── handlers_api.go                # REST API handlers
├── handlers_ui.go                 # Web UI + HTMX partial handlers
├── templates/                     # Go HTML templates (embedded)
│   ├── layout.html
│   ├── wan_interfaces.html
│   ├── wan_interface_form.html
│   ├── actions.html
│   ├── action_form.html
│   ├── history.html
│   ├── settings.html
│   └── partials/
│       ├── wan_interface_card.html
│       ├── network_diagnostics.html
│       ├── egress_fields.html
│       ├── cloudflare_dns_fields.html
│       ├── config_fields.html
│       └── test_route_result.html
├── static/                        # CSS + HTMX 2.0.4 (embedded)
├── Dockerfile
├── docker-compose.yml
└── .github/workflows/
    ├── docker-publish.yml         # main + v* tags → latest / X.Y.Z / X.Y
    └── docker-beta.yml            # v2-dev → beta / v2-dev / beta-<sha>
```

## Issues & Roadmap

File bugs / feature requests at [github.com/AC13139/wan-ip-check/issues](https://github.com/AC13139/wan-ip-check/issues). Current open threads include IPv6 support (#28), event-history auto-recycling (#29), Settings About card (#30), timezone dropdown (#31), and the dashboard redesign (#27).

## License

[GNU General Public License v3.0](LICENSE).
