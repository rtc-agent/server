package httphandler

import (
	"context"
	"net/http"
	"time"

	"github.com/rtc-agent/server/internal/infra/httputil"
	"github.com/rtc-agent/server/internal/svc"
	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"
)

// Handler HTTP 处理器
type Handler struct {
	svcCtx *svc.ServiceContext
}

// NewHandler 创建 HTTP 处理器
func NewHandler(svcCtx *svc.ServiceContext) *Handler {
	return &Handler{svcCtx: svcCtx}
}

// Healthz 健康检查端点（存活探针）
// 仅返回服务是否运行，不检查依赖。用于 K8s livenessProbe。
func (h *Handler) Healthz(w http.ResponseWriter, r *http.Request) {
	httputil.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Readyz 就绪检查端点（就绪探针）
// 检查 DB、Redis、Centrifuge 等依赖是否可用。用于 K8s readinessProbe。
func (h *Handler) Readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	checks := make(map[string]string)
	allReady := true

	// 检查数据库连通性
	if err := h.svcCtx.DB.WithContext(ctx).Exec("SELECT 1").Error; err != nil {
		checks["db"] = "error"
		allReady = false
		logger.Warn(ctx, "Readyz: DB check failed", zap.Error(err))
	} else {
		checks["db"] = "ok"
	}

	// 检查 Redis 连通性
	if err := h.svcCtx.Redis.Ping(ctx).Err(); err != nil {
		checks["redis"] = "error"
		allReady = false
		logger.Warn(ctx, "Readyz: Redis check failed", zap.Error(err))
	} else {
		checks["redis"] = "ok"
	}

	// 检查 Centrifuge 节点状态
	if h.svcCtx.CentrifugeNode == nil {
		checks["centrifuge"] = "not configured"
		allReady = false
	} else {
		// Centrifuge node 没有直接的 Ping 方法，通过检查 node 是否 running 来判断
		// 如果 node 已经 shutdown，NodeInfo() 会返回错误
		if _, err := h.svcCtx.CentrifugeNode.Info(); err != nil {
			checks["centrifuge"] = "error"
			allReady = false
			logger.Warn(ctx, "Readyz: Centrifuge check failed", zap.Error(err))
		} else {
			checks["centrifuge"] = "ok"
		}
	}

	status := http.StatusOK
	response := map[string]any{"status": "ready", "checks": checks}

	if !allReady {
		status = http.StatusServiceUnavailable
		response["status"] = "not ready"
		logger.Warn(ctx, "Readyz check failed", zap.Any("checks", checks))
	}

	httputil.WriteJSON(w, status, response)
}
