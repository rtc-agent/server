package httphandler

import (
	"net/http"

	"github.com/rtc-agent/server/internal/infra/httputil"
	"github.com/rtc-agent/server/internal/svc"
)

// Handler HTTP 处理器
type Handler struct {
	svcCtx *svc.ServiceContext
}

// NewHandler 创建 HTTP 处理器
func NewHandler(svcCtx *svc.ServiceContext) *Handler {
	return &Handler{svcCtx: svcCtx}
}

// Healthz 健康检查端点
func (h *Handler) Healthz(w http.ResponseWriter, r *http.Request) {
	httputil.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Readyz 就绪检查端点（可扩展：检查 DB 连通性）
func (h *Handler) Readyz(w http.ResponseWriter, r *http.Request) {
	// TODO: 检查 DB、Redis 等依赖是否就绪（待接入健康检查 issue）
	httputil.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
