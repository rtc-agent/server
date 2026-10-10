// Package middleware provides a collection of HTTP middleware.
//
// Includes CORS, security headers, request logging, JWT authentication, etc.
// The Chain function composes multiple middleware in onion-model order.
package middleware

import "net/http"

// Middleware is the standard HTTP middleware signature.
type Middleware func(http.Handler) http.Handler

// Chain composes multiple middleware in order.
// Chain(a, b, c)(handler) = a(b(c(handler)))
// The first middleware is the outermost (executes first); the last is closest to the handler.
func Chain(middlewares ...Middleware) Middleware {
	return func(final http.Handler) http.Handler {
		for i := len(middlewares) - 1; i >= 0; i-- {
			final = middlewares[i](final)
		}
		return final
	}
}

// StandardChain composes middleware in the recommended security-first order.
//
// The order is critical for security and correctness:
//  1. Recovery       - panic recovery must be outermost to catch all panics
//  2. RequestID      - assign request ID early for tracing
//  3. SecurityHeaders - set security headers (CSP, HSTS, etc.) before any response
//  4. CORS           - handle CORS preflight before auth
//  5. RateLimit      - rate limit before expensive auth checks
//  6. Auth           - authentication before authorization
//  7. Audit          - audit logging after auth (captures user identity)
//
// Nil middleware are skipped, allowing conditional inclusion:
//
//	h := StandardChain(
//	    Recovery(),
//	    nil, // skip RequestID if not needed
//	    SecurityHeaders(),
//	    nil, // skip CORS for internal API
//	    RateLimit(),
//	    Auth(),
//	    Audit(),
//	)(handler)
//
// This is a convenience wrapper around Chain that enforces the recommended order.
// For custom ordering, use Chain directly.
func StandardChain(
	recovery Middleware,
	requestID Middleware,
	securityHeaders Middleware,
	cors Middleware,
	rateLimit Middleware,
	auth Middleware,
	audit Middleware,
) Middleware {
	middlewares := make([]Middleware, 0, 7)
	// Add in recommended order, skipping nil entries.
	if recovery != nil {
		middlewares = append(middlewares, recovery)
	}
	if requestID != nil {
		middlewares = append(middlewares, requestID)
	}
	if securityHeaders != nil {
		middlewares = append(middlewares, securityHeaders)
	}
	if cors != nil {
		middlewares = append(middlewares, cors)
	}
	if rateLimit != nil {
		middlewares = append(middlewares, rateLimit)
	}
	if auth != nil {
		middlewares = append(middlewares, auth)
	}
	if audit != nil {
		middlewares = append(middlewares, audit)
	}
	return Chain(middlewares...)
}

// MustStandardChain is like StandardChain but panics if no middleware are provided.
// Use this when at least recovery and auth middleware are required.
func MustStandardChain(middlewares ...Middleware) Middleware {
	if len(middlewares) == 0 {
		panic("middleware: MustStandardChain requires at least one middleware")
	}
	return Chain(middlewares...)
}
