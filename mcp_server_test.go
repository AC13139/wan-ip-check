package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// newTestMCPServer wires an MCPServer against a tempdir SQLite + an
// in-memory action registry. The registry has a fake Cloudflare DNS
// executor whose SecretFields lists api_token so we can prove the
// masking survives the MCP round-trip.
func newTestMCPServer(t *testing.T) *MCPServer {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := OpenDB(t.TempDir(), logger)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	reg := NewActionRegistry()
	reg.Register(&fakeCFExecutor{})

	app := &App{
		db:       db,
		registry: reg,
		logger:   logger,
	}
	return NewMCPServer(app)
}

// fakeCFExecutor is the minimal ActionExecutor we need; the SecretFields
// declaration is what maskSecrets consults.
type fakeCFExecutor struct{}

func (*fakeCFExecutor) Type() string                 { return "cloudflare_dns" }
func (*fakeCFExecutor) DisplayName() string          { return "CF DNS" }
func (*fakeCFExecutor) Description() string          { return "" }
func (*fakeCFExecutor) SecretFields() []string      { return []string{"api_token"} }
func (*fakeCFExecutor) DefaultConfig() json.RawMessage {
	return json.RawMessage(`{}`)
}
func (*fakeCFExecutor) ConfigSchema() []ConfigField { return nil }
func (*fakeCFExecutor) ValidateConfig(json.RawMessage) error { return nil }
func (*fakeCFExecutor) Execute(context.Context, IPEvent, json.RawMessage) error {
	return nil
}

// seedAction writes an action whose api_token we will then try to leak
// through list_actions. Returns the action ID.
func seedAction(t *testing.T, m *MCPServer) int64 {
	t.Helper()
	id, err := m.app.db.CreateAction(&Action{
		Type:       "cloudflare_dns",
		Name:       "Test CF",
		Enabled:    true,
		ConfigJSON: json.RawMessage(`{"api_token":"super-secret-token-do-not-leak","zone_id":"abc123","records":[{"name":"foo"}]}`),
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	return id
}

// TestMCPListActionsMasksSecrets verifies the Phase 1 acceptance
// criterion: no api_token (or any IsSecret field) appears in MCP output.
func TestMCPListActionsMasksSecrets(t *testing.T) {
	m := newTestMCPServer(t)
	seedAction(t, m)

	res, err := m.handleListActions(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatalf("handleListActions: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error: %+v", res)
	}

	// Walk every text content block and assert the secret is not present.
	for _, c := range res.Content {
		text := ""
		if tc, ok := c.(mcp.TextContent); ok {
			text = tc.Text
		}
		if strings.Contains(text, "super-secret-token-do-not-leak") {
			t.Errorf("api_token leaked through list_actions response:\n%s", text)
		}
	}
	// Also assert structuredContent — modern clients consume this directly.
	if res.StructuredContent != nil {
		data, _ := json.Marshal(res.StructuredContent)
		if strings.Contains(string(data), "super-secret-token-do-not-leak") {
			t.Errorf("api_token leaked through structuredContent:\n%s", string(data))
		}
	}
}

// TestMCPReadOnlyAnnotations verifies every registered Phase 1 tool
// declares readOnlyHint=true and destructiveHint=false per the
// 2026-07-28 spec. Refusing to mark these would let a UI confidently
// render a "modify" button on read-only operations.
func TestMCPReadOnlyAnnotations(t *testing.T) {
	m := newTestMCPServer(t)
	tools := m.mcp.ListTools()
	if len(tools) == 0 {
		t.Fatal("no tools registered")
	}

	expected := []string{
		"list_wan_interfaces",
		"get_wan_status",
		"list_actions",
		"get_recent_history",
		"get_failed_actions",
		"get_settings",
	}
	for _, name := range expected {
		t1, ok := tools[name]
		if !ok {
			t.Errorf("expected tool %q registered", name)
			continue
		}
		ann := t1.Tool.Annotations
		if ann.ReadOnlyHint == nil || !*ann.ReadOnlyHint {
			t.Errorf("tool %q missing ReadOnlyHint=true", name)
		}
		if ann.DestructiveHint == nil || *ann.DestructiveHint {
			t.Errorf("tool %q should set DestructiveHint=false (was %v)", name, ann.DestructiveHint)
		}
		if ann.IdempotentHint == nil || !*ann.IdempotentHint {
			t.Errorf("tool %q missing IdempotentHint=true", name)
		}
		if ann.Title == "" {
			t.Errorf("tool %q missing Title annotation", name)
		}
	}
}

// TestMCPErrorsSetIsError verifies that tool execution errors return
// isError: true (per spec "Tool Execution Errors" section) — distinct
// from JSON-RPC protocol errors. Clients route these differently.
func TestMCPErrorsSetIsError(t *testing.T) {
	m := newTestMCPServer(t)

	res, err := m.handleGetWANStatus(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "get_wan_status", Arguments: map[string]any{"wan_id": float64(99999)}},
	})
	if err != nil {
		t.Fatalf("handleGetWANStatus returned Go error: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError=true for missing WAN, got: %+v", res)
	}
}

// TestMCPInputValidation — spec § Security: "Validate all tool inputs".
// A non-positive wan_id must be rejected before any DB call.
func TestMCPInputValidation(t *testing.T) {
	m := newTestMCPServer(t)

	for _, bad := range []float64{0, -1, -1000} {
		res, err := m.handleGetWANStatus(context.Background(), mcp.CallToolRequest{
			Params: mcp.CallToolParams{Name: "get_wan_status", Arguments: map[string]any{"wan_id": bad}},
		})
		if err != nil {
			t.Fatalf("wan_id=%v returned Go error: %v", bad, err)
		}
		if !res.IsError {
			t.Errorf("wan_id=%v should be rejected with IsError=true", bad)
		}
	}
}

// TestBearerAuth — Phase 1 acceptance criterion: bearer token enforced
// when configured; dev mode (empty token) passes through.
func TestBearerAuth(t *testing.T) {
	mux := httpHandlerForToken("secret-token")

	// 1. No token — 401.
	r1 := httptest.NewRecorder()
	mux.ServeHTTP(r1, httptest.NewRequest("GET", "/mcp", nil))
	if r1.Code != 401 {
		t.Errorf("missing token: want 401, got %d", r1.Code)
	}

	// 2. Wrong token — 401.
	r2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/mcp", nil)
	req2.Header.Set("Authorization", "Bearer wrong")
	mux.ServeHTTP(r2, req2)
	if r2.Code != 401 {
		t.Errorf("wrong token: want 401, got %d", r2.Code)
	}

	// 3. Right token — passes through (handler is a stub).
	r3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("GET", "/mcp", nil)
	req3.Header.Set("Authorization", "Bearer secret-token")
	mux.ServeHTTP(r3, req3)
	if r3.Code == 401 {
		t.Errorf("right token should pass, got 401")
	}

	// 4. Empty token (dev mode) — open.
	openMux := httpHandlerForToken("")
	r4 := httptest.NewRecorder()
	openMux.ServeHTTP(r4, httptest.NewRequest("GET", "/mcp", nil))
	if r4.Code == 401 {
		t.Errorf("dev mode (empty token) should not 401")
	}
}

// httpHandlerForToken wraps a stub pass-through handler in the same
// bearerAuth used by ServeHTTP. Lets us test the middleware without
// standing up the full MCP server.
func httpHandlerForToken(token string) http.Handler {
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	return bearerAuth(token, stub)
}
