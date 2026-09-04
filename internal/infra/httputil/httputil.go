// Package httputil 提供 HTTP 响应的公共辅助函数。
//
// 消除 oauth2、mockoauth2 等包中重复的 JSON 响应写入逻辑。
package httputil

import (
	"encoding/json"
	"net/http"
)

// WriteJSON 写入 JSON 响应。
//
// 设置 Content-Type 为 application/json，写入状态码，编码 data 到响应体。
// 编码失败时静默忽略（响应头已发送，无法改变状态码）。
func WriteJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// WriteError 写入结构化错误响应。
//
// 响应格式为 {"error": errCode, "error_description": description}，
// 同时设置 Cache-Control / Pragma 头防止客户端缓存错误响应。
func WriteError(w http.ResponseWriter, status int, errCode, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	resp := map[string]string{
		"error": errCode,
	}
	if description != "" {
		resp["error_description"] = description
	}
	_ = json.NewEncoder(w).Encode(resp)
}
