// Package admin provides the admin-server cobra command and initialization.
package admin

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

//go:generate sh -c "cd ../../../web-components/packages/admin-ui && pnpm install && pnpm run build"
//go:generate sh -c "rm -rf web/admin && mkdir -p web/admin && cp -r ../../../web-components/packages/admin-ui/dist/* web/admin/"

//go:embed all:web/admin
var adminUI embed.FS

// ServeStaticFiles 设置静态文件服务和 SPA 路由
// 所有非 API 路由都会 fallback 到 index.html，支持前端路由
func ServeStaticFiles(app *gin.Engine) {
	// 从 embed.FS 创建子文件系统（去掉 "web/admin" 前缀）
	distFS, err := fs.Sub(adminUI, "web/admin")
	if err != nil {
		panic("failed to create sub filesystem: " + err.Error())
	}

	// 使用 NoRoute 处理所有未匹配的路由
	app.NoRoute(func(c *gin.Context) {
		// API 路由不处理，返回 404
		if strings.HasPrefix(c.Request.URL.Path, "/api") {
			c.JSON(http.StatusNotFound, gin.H{
				"error":             "not_found",
				"error_description": "API endpoint not found",
			})
			return
		}

		// 健康检查路由不处理
		if c.Request.URL.Path == "/health" {
			c.JSON(http.StatusNotFound, gin.H{
				"error":             "not_found",
				"error_description": "Health endpoint not found",
			})
			return
		}

		// 尝试提供请求的文件
		path := strings.TrimPrefix(c.Request.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}

		// 设置缓存控制头
		if path == "index.html" {
			// index.html 不缓存，确保每次都获取最新版本
			c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
			c.Header("Pragma", "no-cache")
			c.Header("Expires", "0")
		} else if strings.Contains(path, "/assets/") {
			// Vite 构建的带 hash 的资源文件，长期缓存
			c.Header("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			// 其他文件，缓存 1 小时
			c.Header("Cache-Control", "public, max-age=3600")
		}

		// 使用 ServeFileFS 代替 ServeContent，因为它支持 embed.FS
		// 如果文件不存在，ServeFileFS 会自动返回 404
		// 对于 SPA，我们需要手动处理 fallback 到 index.html
		if _, err := fs.Stat(distFS, path); err == nil {
			// 文件存在，直接提供
			http.ServeFileFS(c.Writer, c.Request, distFS, path)
			return
		}

		// 文件不存在，返回 index.html（SPA 路由）
		c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
		c.Header("Pragma", "no-cache")
		c.Header("Expires", "0")
		http.ServeFileFS(c.Writer, c.Request, distFS, "index.html")
	})
}
