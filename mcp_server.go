package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// MCPServer wraps the mark3labs MCP server with the wan-ip-check app's
// dependencies. It is constructed once in main() and started alongside
// the regular HTTP server. stdio is the default transport so Claude Code
// can launch it directly; HTTP is also started if MCP_HTTP_PORT > 0.
type MCPServer struct {
	app    *App
	mcp    *server.MCPServer
	logger *slog.Logger
}

// NewMCPServer registers all read-only tools against a fresh MCP server.
// Phase 2 (#37) will add trigger_check + execute_action.
func NewMCPServer(app *App) *MCPServer {
	srv := server.NewMCPServer(
		"wan-ip-check",
		"2.0.0",
		server.WithToolCapabilities(false),
		server.WithLogging(),
	)

	m := &MCPServer{
		app:    app,
		mcp:    srv,
		logger: app.logger,
	}
	m.registerTools()
	return m
}

func (m *MCPServer) registerTools() {
	// Per 2026-07-28 spec, all 6 Phase 1 tools are read-only:
	// - WithReadOnlyHintAnnotation(true)  : client UI can render a "read-only" badge
	// - WithDestructiveHintAnnotation(false): tells clients these don't mutate state
	// - WithIdempotentHintAnnotation(true) : safe to retry on transient failures
	// - WithOpenWorldHintAnnotation(false) : data comes from this app's DB only
	// - title                                : human-readable display name
	// - outputSchema                         : lets clients validate structured results
	m.mcp.AddTool(
		mcp.NewTool("list_wan_interfaces",
			mcp.WithDescription("List all WAN interfaces with current state (IP, status, last check, last error). Returns every WAN regardless of enabled flag."),
			mcp.WithTitleAnnotation("List WAN Interfaces"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
		),
		m.handleListWANInterfaces,
	)

	m.mcp.AddTool(
		mcp.NewTool("get_wan_status",
			mcp.WithDescription("Full status for one WAN interface including recent IP-change history and last error."),
			mcp.WithTitleAnnotation("Get WAN Status"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithNumber("wan_id",
				mcp.Required(),
				mcp.Description("Numeric WAN interface ID."),
			),
		),
		m.handleGetWANStatus,
	)

	m.mcp.AddTool(
		mcp.NewTool("list_actions",
			mcp.WithDescription("List all configured actions (Discord, Cloudflare DNS, Cloudflare Gateway, Webhook). Secrets (api_token, webhook_url) are masked."),
			mcp.WithTitleAnnotation("List Actions"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
		),
		m.handleListActions,
	)

	m.mcp.AddTool(
		mcp.NewTool("get_recent_history",
			mcp.WithDescription("Recent check + action-run history for one WAN. Returns merged timeline like the History page."),
			mcp.WithTitleAnnotation("Get Recent History"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithNumber("wan_id",
				mcp.Required(),
				mcp.Description("Numeric WAN interface ID."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Max rows to return (1-500). Defaults to 50."),
			),
		),
		m.handleGetRecentHistory,
	)

	m.mcp.AddTool(
		mcp.NewTool("get_failed_actions",
			mcp.WithDescription("Action runs that are still queued for retry (needs_retry=1) or that have been abandoned after max retries. Use this to spot attention-needing items."),
			mcp.WithTitleAnnotation("Get Failed Actions"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
		),
		m.handleGetFailedActions,
	)

	m.mcp.AddTool(
		mcp.NewTool("get_settings",
			mcp.WithDescription("Get non-secret runtime settings (daily summary time, timezone, retention TTLs). Does not include API tokens or webhooks."),
			mcp.WithTitleAnnotation("Get Settings"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
		),
		m.handleGetSettings,
	)
}

// ServeStdio runs the MCP server on stdin/stdout for direct CLI agent use.
// Blocks until the parent context is cancelled or stdin closes.
func (m *MCPServer) ServeStdio(ctx context.Context) error {
	m.logger.Info("MCP stdio server starting")
	return server.ServeStdio(m.mcp)
}

// Handler returns the bearer-auth-wrapped MCP HTTP handler. The main
// HTTP server registers it on its own mux (before the CSRF catch-all),
// so the MCP endpoint is reachable on the same port as the Web UI —
// no reverse-proxy tweaks required.
//
// Bearer token enforced via middleware unless the env var is empty
// (dev mode only — logs a warning). The token is read from the same
// MCP_AUTH_TOKEN env var as before.
func (m *MCPServer) Handler() http.Handler {
	token := os.Getenv("MCP_AUTH_TOKEN")
	if token == "" {
		m.logger.Warn("MCP_AUTH_TOKEN unset — MCP endpoint is unauthenticated. Set MCP_AUTH_TOKEN before exposing it.")
	}

	streamable := server.NewStreamableHTTPServer(m.mcp,
		server.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			return ctx
		}),
	)

	m.logger.Info("MCP HTTP handler ready", "auth", token != "")
	return bearerAuth(token, streamable)
}

// Healthz returns a tiny readiness handler for the MCP server. Useful as
// a reverse-proxy healthcheck independent of any registered MCP route.
func (m *MCPServer) Healthz() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
}