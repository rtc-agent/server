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
