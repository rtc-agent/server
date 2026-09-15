package middleware

import (
	"crypto/subtle"
	"net/http"
	"net/http/pprof"
)

// RegisterPprofRoutes 在给定 mux 上注册 /debug/pprof/* 路由。
//
// 注册的路径：
//   - /debug/pprof/          索引页（列出可用 profiles）
//   - /debug/pprof/cmdline   命令行参数
//   - /debug/pprof/profile   CPU profile（默认 30s）
//   - /debug/pprof/symbol    符号查找
//   - /debug/pprof/trace     执行 trace
//   - /debug/pprof/{name}    任意已注册 profile（heap, goroutine, allocs 等）
//
// 注意：调用方应在路由注册前包裹认证中间件（如 BasicAuth），
// 避免生产环境暴露内部状态。
func RegisterPprofRoutes(mux *http.ServeMux) {
	// 索引页使用 pprof.Index（标准库自带索引，列出所有 profile）
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
}

// PprofHandler 返回一个包裹了认证中间件的 pprof HTTP handler。
// 用于在生产环境中为 /debug/pprof/ 前缀下的所有请求添加 basic auth。
//
// 用法示例（在 server.go 中）：
//
//	mux.Handle("/debug/pprof/", middleware.PprofHandler(user, password))
func PprofHandler(user, password string) http.Handler {
	pprofMux := http.NewServeMux()
	RegisterPprofRoutes(pprofMux)
	return BasicAuth(pprofMux, user, password)
}

// BasicAuth HTTP Basic Authentication 中间件（导出函数）。
// 使用常量时间比较防止时序攻击。
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
