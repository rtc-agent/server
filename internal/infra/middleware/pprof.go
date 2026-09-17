package middleware

import (
	"crypto/subtle"
	"net/http"
	"net/http/pprof"
)

// RegisterPprofRoutes registers /debug/pprof/* routes on the given mux.
//
// Registered paths:
//   - /debug/pprof/          Index page (lists available profiles)
//   - /debug/pprof/cmdline   Command-line arguments
//   - /debug/pprof/profile   CPU profile (default 30s)
//   - /debug/pprof/symbol    Symbol lookup
//   - /debug/pprof/trace     Execution trace
//   - /debug/pprof/{name}    Any registered profile (heap, goroutine, allocs, etc.)
//
// NOTE: callers should wrap an authentication middleware (e.g. BasicAuth)
// before registering these routes to avoid exposing internal state in production.
func RegisterPprofRoutes(mux *http.ServeMux) {
	// Index page uses pprof.Index (standard library index, lists all profiles).
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
}

// PprofHandler returns a pprof HTTP handler wrapped with authentication middleware.
// Used to add basic auth to all /debug/pprof/ requests in production.
//
// Usage example (in server.go):
//
//	mux.Handle("/debug/pprof/", middleware.PprofHandler(user, password))
func PprofHandler(user, password string) http.Handler {
	pprofMux := http.NewServeMux()
	RegisterPprofRoutes(pprofMux)
	return BasicAuth(pprofMux, user, password)
}

// BasicAuth is an HTTP Basic Authentication middleware.
// Uses constant-time comparison to prevent timing attacks.
func BasicAuth(next http.Handler, user, password string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="debug"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
