package main

import (
	"crypto/subtle"
	"net/http"
)

// bearerAuth wraps a handler with optional bearer-token authentication.
// If token is empty, the wrapper is a no-op (caller is responsible for
// logging a warning at startup — ServeHTTP does this).
//
// Constant-time comparison via subtle.ConstantTimeCompare avoids timing
// oracles. Comparison only happens when a token is configured, so dev
// mode (empty token) is genuinely open.
func bearerAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}

		got := bearerFromRequest(r)
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerFromRequest(r *http.Request) string {
	// Standard Authorization header first.
	if h := r.Header.Get("Authorization"); h != "" {
		const prefix = "Bearer "
		if len(h) > len(prefix) && h[:len(prefix)] == prefix {
			return h[len(prefix):]
		}
		return h
	}
	// Fallback: ?token=... (some MCP clients prefer query string).
	if t := r.URL.Query().Get("token"); t != "" {
		return t
	}
	return ""
}