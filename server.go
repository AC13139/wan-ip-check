package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Server holds the HTTP server and its dependencies.
type Server struct {
	app        *App
	dispatcher *Dispatcher
	templates  *Templates
	httpServer *http.Server
	logger     *slog.Logger
}

// NewServer creates a new HTTP server with all routes registered.
func NewServer(app *App, dispatcher *Dispatcher) (*Server, error) {
	templates, err := loadTemplates()
	if err != nil {
		return nil, fmt.Errorf("loading templates: %w", err)
	}

	s := &Server{
		app:        app,
		dispatcher: dispatcher,
		templates:  templates,
		logger:     app.logger,
	}

	mux := http.NewServeMux()

	// Static files
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticFileServer()))

	// API routes (JSON, no CSRF)
	s.registerAPIRoutes(mux)

	// UI routes (HTML, CSRF-protected)
	uiMux := http.NewServeMux()
	s.registerUIRoutes(uiMux)
	mux.Handle("/", s.csrfMiddleware(uiMux))

	s.httpServer = &http.Server{
		Addr:         ":" + app.boot.Port,
		Handler:      logMiddleware(s.logger, mux),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	return s, nil
}

// Start begins listening. Blocks until the server stops.
func (s *Server) Start() error {
	s.logger.Info("HTTP server listening", "addr", s.httpServer.Addr)
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("HTTP server error: %w", err)
	}
	return nil
}

// Shutdown gracefully stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

// registerAPIRoutes sets up all /api/* endpoints (JSON, no CSRF).
func (s *Server) registerAPIRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/status", s.handleGetStatus)
	mux.HandleFunc("GET /api/wan-interfaces", s.handleListWANInterfaces)
	mux.HandleFunc("POST /api/wan-interfaces", s.handleCreateWANInterface)
	mux.HandleFunc("GET /api/wan-interfaces/{id}", s.handleGetWANInterface)
	mux.HandleFunc("PUT /api/wan-interfaces/{id}", s.handleUpdateWANInterface)
	mux.HandleFunc("DELETE /api/wan-interfaces/{id}", s.handleDeleteWANInterface)
	mux.HandleFunc("POST /api/wan-interfaces/{id}/check", s.handleManualCheck)
	mux.HandleFunc("GET /api/wan-interfaces/{id}/history", s.handleWANInterfaceHistory)
	mux.HandleFunc("GET /api/actions", s.handleListActions)
	mux.HandleFunc("POST /api/actions", s.handleCreateAction)
	mux.HandleFunc("GET /api/actions/{id}", s.handleGetAction)
	mux.HandleFunc("PUT /api/actions/{id}", s.handleUpdateAction)
	mux.HandleFunc("DELETE /api/actions/{id}", s.handleDeleteAction)
	mux.HandleFunc("POST /api/actions/{id}/test", s.handleTestAction)
	mux.HandleFunc("GET /api/action-types", s.handleListActionTypes)
	mux.HandleFunc("GET /api/wan-interfaces/{id}/actions", s.handleGetWANInterfaceActions)
	mux.HandleFunc("PUT /api/wan-interfaces/{id}/actions", s.handleSetWANInterfaceActions)
	mux.HandleFunc("GET /api/network/interfaces", s.handleNetworkInterfaces)
	mux.HandleFunc("GET /api/network/capabilities", s.handleNetworkCapabilities)
	mux.HandleFunc("GET /api/network/diagnostics", s.handleNetworkDiagnostics)
	mux.HandleFunc("GET /api/history", s.handleGlobalHistory)
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handleUpdateSettings)
}

// registerUIRoutes sets up all UI page + HTMX partial endpoints.
func (s *Server) registerUIRoutes(mux *http.ServeMux) {
	// Full pages
	mux.HandleFunc("GET /{$}", s.handleWANInterfacesPage)
	mux.HandleFunc("GET /wan-interfaces", s.handleWANInterfacesPage)
	mux.HandleFunc("GET /wan-interfaces/new", s.handleWANInterfaceForm)
	mux.HandleFunc("GET /wan-interfaces/{id}", s.handleWANInterfaceForm)
	mux.HandleFunc("GET /actions", s.handleActionsPage)
	mux.HandleFunc("GET /actions/new", s.handleActionForm)
	mux.HandleFunc("GET /actions/{id}", s.handleActionForm)
	mux.HandleFunc("GET /history", s.handleHistoryPage)
	mux.HandleFunc("GET /settings", s.handleSettingsPage)

	// HTMX partials — form submissions
	mux.HandleFunc("POST /ui/wan-interfaces", s.handleUICreateWANInterface)
	mux.HandleFunc("POST /ui/wan-interfaces/{id}", s.handleUIUpdateWANInterface)
	mux.HandleFunc("POST /ui/actions", s.handleUICreateAction)
	mux.HandleFunc("POST /ui/actions/{id}", s.handleUIUpdateAction)
	mux.HandleFunc("PUT /ui/settings", s.handleUIUpdateSettings)

	// HTMX partials — inline actions
	mux.HandleFunc("POST /ui/wan-interfaces/{id}/check", s.handleUIManualCheck)
	mux.HandleFunc("POST /ui/wan-interfaces/{id}/toggle", s.handleUIToggleWANInterface)
	mux.HandleFunc("POST /ui/actions/{id}/toggle", s.handleUIToggleAction)
	mux.HandleFunc("POST /ui/actions/{id}/execute", s.handleUIExecuteAction)
	mux.HandleFunc("POST /ui/action-test", s.handleUITestAction)
	mux.HandleFunc("POST /ui/cloudflare/zones", s.handleUICloudflareZones)
	mux.HandleFunc("DELETE /ui/wan-interfaces/{id}", s.handleUIDeleteWANInterface)
	mux.HandleFunc("DELETE /ui/actions/{id}", s.handleUIDeleteAction)

	// HTMX partials — data loading
	mux.HandleFunc("GET /ui/egress-fields", s.handleUIEgressFields)
	mux.HandleFunc("GET /ui/action-schema", s.handleUIActionSchema)
	mux.HandleFunc("GET /ui/history-table", s.handleUIHistoryTable)
	mux.HandleFunc("GET /ui/network-diagnostics", s.handleUINetworkDiagnostics)
	mux.HandleFunc("POST /ui/test-route", s.handleUITestRoute)
}

// --- Middleware ---

func logMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		logger.Debug("HTTP request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration", time.Since(start).Round(time.Millisecond),
		)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// --- JSON helpers ---

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeJSON(r *http.Request, v interface{}) error {
	limited := io.LimitReader(r.Body, 1<<20)
	defer r.Body.Close()
	if err := json.NewDecoder(limited).Decode(v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}
