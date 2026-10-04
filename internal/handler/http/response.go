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

// Error 错误响应
func Error(c *gin.Context, statusCode int, errorCode, errorMessage string) {
	c.JSON(statusCode, ResponseStructure{
		Success:      false,
		ErrorCode:    errorCode,
		ErrorMessage: errorMessage,
	})
}
