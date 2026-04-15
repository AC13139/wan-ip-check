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
	httpServer *http.Server
	logger     *slog.Logger
}

// NewServer creates a new HTTP server with all routes registered.
func NewServer(app *App, dispatcher *Dispatcher) *Server {
	s := &Server{
		app:        app,
		dispatcher: dispatcher,
		logger:     app.logger,
	}

	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	// TODO Phase 4: register UI routes (templates + HTMX)
	// Placeholder root redirect
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			writeJSON(w, http.StatusOK, map[string]string{
				"app":     "wan-ip-check",
				"version": "2.0.0-alpha",
				"ui":      "coming in Phase 4",
			})
			return
		}
		http.NotFound(w, r)
	})

	s.httpServer = &http.Server{
		Addr:         ":" + app.boot.Port,
		Handler:      logMiddleware(s.logger, mux),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	return s
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

// registerAPIRoutes sets up all /api/* endpoints.
func (s *Server) registerAPIRoutes(mux *http.ServeMux) {
	// Status
	mux.HandleFunc("GET /api/status", s.handleGetStatus)

	// Sources
	mux.HandleFunc("GET /api/sources", s.handleListSources)
	mux.HandleFunc("POST /api/sources", s.handleCreateSource)
	mux.HandleFunc("GET /api/sources/{id}", s.handleGetSource)
	mux.HandleFunc("PUT /api/sources/{id}", s.handleUpdateSource)
	mux.HandleFunc("DELETE /api/sources/{id}", s.handleDeleteSource)
	mux.HandleFunc("POST /api/sources/{id}/check", s.handleManualCheck)
	mux.HandleFunc("GET /api/sources/{id}/history", s.handleSourceHistory)

	// Actions
	mux.HandleFunc("GET /api/actions", s.handleListActions)
	mux.HandleFunc("POST /api/actions", s.handleCreateAction)
	mux.HandleFunc("GET /api/actions/{id}", s.handleGetAction)
	mux.HandleFunc("PUT /api/actions/{id}", s.handleUpdateAction)
	mux.HandleFunc("DELETE /api/actions/{id}", s.handleDeleteAction)
	mux.HandleFunc("POST /api/actions/{id}/test", s.handleTestAction)
	mux.HandleFunc("GET /api/action-types", s.handleListActionTypes)

	// Source-Action linking
	mux.HandleFunc("GET /api/sources/{id}/actions", s.handleGetSourceActions)
	mux.HandleFunc("PUT /api/sources/{id}/actions", s.handleSetSourceActions)

	// Global
	mux.HandleFunc("GET /api/history", s.handleGlobalHistory)
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handleUpdateSettings)
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
	// Limit body to 1MB to prevent memory abuse
	limited := io.LimitReader(r.Body, 1<<20)
	defer r.Body.Close()
	if err := json.NewDecoder(limited).Decode(v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}
