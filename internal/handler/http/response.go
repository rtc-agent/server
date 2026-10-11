// Package httphandler provides HTTP handler implementations.
//
// # API Error Handling Architecture Decision
//
// This project uses HTTP 200 + application-level error codes for all API responses.
// This is an intentional design choice that differs from REST conventions (which would
// use HTTP status codes like 400, 404, 500 for errors).
//
// Rationale:
//   - Simplifies frontend error handling: all responses have the same structure
//   - Enables richer error information: errorCode + errorMessage provide context
//   - Avoids HTTP status code ambiguity: 200 always means "request processed"
//   - Better integration with Ant Design Pro frontend conventions
//   - Easier to implement retry logic: check success field, not HTTP status
//
// Response structure:
//
//	Success case:
//	  {
//	    "success": true,
//	    "data": { ... }
//	  }
//
//	Error case:
//	  {
//	    "success": false,
//	    "errorCode": "VALIDATION_ERROR",
//	    "errorMessage": "Invalid email format"
//	  }
//
// Error code conventions:
//   - Use UPPER_SNAKE_CASE for error codes (e.g., "VALIDATION_ERROR", "NOT_FOUND")
//   - Define error codes in a centralized location (see internal/domain/errors.go)
//   - Include human-readable errorMessage for debugging and user display
//   - Keep errorCode stable for programmatic handling, errorMessage can change
//
// Exception: Health check endpoints use standard HTTP status codes (200, 503) because
// they are consumed by infrastructure tools (load balancers, Kubernetes) that expect
// standard HTTP semantics.
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

// Error 错误响应 - 始终使用 HTTP 200，通过 success=false 和 errorCode 传达错误信息
// 设计要求：所有 API 错误通过 HTTP 200 + { success: false, errorCode: "..." } 传达
func Error(c *gin.Context, errorCode, errorMessage string) {
	c.JSON(http.StatusOK, ResponseStructure{
		Success:      false,
		ErrorCode:    errorCode,
		ErrorMessage: errorMessage,
	})
}
