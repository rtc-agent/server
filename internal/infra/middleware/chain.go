// Package middleware 提供 HTTP 中间件集合。
//
// 包含 CORS、安全响应头、请求日志、JWT 鉴权等中间件。
// 通过 Chain 函数可组合多个中间件，按洋葱模型依次执行。
package middleware

import "net/http"

// Middleware 标准 HTTP 中间件签名。
type Middleware func(http.Handler) http.Handler

// Chain 将多个中间件按顺序组合。
// Chain(a, b, c)(handler) = a(b(c(handler)))
// 第一个中间件最外层（最先执行），最后一个最接近 handler。
func Chain(middlewares ...Middleware) Middleware {
	return func(final http.Handler) http.Handler {
		for i := len(middlewares) - 1; i >= 0; i-- {
			final = middlewares[i](final)
		}
		return final
	}
}
