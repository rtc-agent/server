// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// ResponseStructure 统一响应格式（与 Ant Design Pro 前端约定）
type ResponseStructure struct {
	Success      bool        `json:"success"`
	Data         interface{} `json:"data,omitempty"`
	ErrorCode    string      `json:"errorCode,omitempty"`
	ErrorMessage string      `json:"errorMessage,omitempty"`
}

// Success 成功响应
func Success(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, ResponseStructure{
		Success: true,
		Data:    data,
	})
}

// Error 错误响应 - 始终返回 200 状态码，通过响应体字段表示错误
// 这样前端的 response interceptor 能够正常拦截和处理错误
// 前端通过 success=false 和 errorCode 来判断错误类型
func Error(c *gin.Context, statusCode int, errorCode, errorMessage string) {
	// 始终返回 200 状态码，让前端的 response interceptor 能够处理
	c.JSON(http.StatusOK, ResponseStructure{
		Success:      false,
		ErrorCode:    errorCode,
		ErrorMessage: errorMessage,
	})
}
