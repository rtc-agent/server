package cmd

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/rtc-agent/server/internal/oauth"
)

// mockOAuth2Cmd Mock OAuth2 服务器命令
var mockOAuth2Cmd = &cobra.Command{
	Use:   "mock-oauth2",
	Short: "启动 Mock OAuth2 服务器",
	Long: `启动一个简化的 Mock OAuth2 服务器，用于开发测试。

功能：
- HTML 授权页面（输入 User ID，点击授权）
- 授权码生成与验证
- 使用 client_id/client_secret 校验换取用户信息

所有数据保存在内存中，重启后清空。仅供开发环境使用。`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// 覆盖父命令的 PersistentPreRunE：本命令使用独立的 viper 实例加载配置，
		// 避免与主 server 的全局 viper 耦合。
		return nil
	},
	Run: func(cmd *cobra.Command, args []string) {
		// 配置文件路径：默认 mock-oauth2 专属配置，--config 显式指定时使用指定路径
		configPath := "etc/mock-oauth2.yaml"
		if f := cmd.Flags().Lookup("config"); f != nil && f.Changed {
			configPath = cfgFile
		}
		runMockOAuth2(configPath)
	},
}

func init() {
	rootCmd.AddCommand(mockOAuth2Cmd)
}

// runMockOAuth2 启动 Mock OAuth2 服务器
func runMockOAuth2(configPath string) {
	// 加载配置（独立 viper 实例）
	v := viper.New()
	v.SetConfigFile(configPath)
	v.SetDefault("server.port", "10060")
	v.SetDefault("client_id", "test-client")
	v.SetDefault("client_secret", "test-client-secret")

	if err := v.ReadInConfig(); err != nil {
		log.Fatalf("读取配置失败: %v", err)
	}

	// 创建 Mock OAuth2 Provider
	provider := oauth.NewProvider(oauth.Config{
		ClientID:     v.GetString("client_id"),
		ClientSecret: v.GetString("client_secret"),
	})

	// 注册路由
	mux := http.NewServeMux()
	provider.RegisterRoutes(mux)

	// 健康检查端点
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "OK")
	})

	// CORS 中间件：开放所有来源（仅 Mock 环境使用）
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		// 预检请求直接返回 204
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mux.ServeHTTP(w, r)
	})

	// 创建 HTTP Server
	port := v.GetString("server.port")
	srv := &http.Server{
		Addr:    ":" + port,
		Handler: handler,
	}

	// 启动 HTTP Server
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("Mock OAuth2 Server panic: %v\n%s", r, debug.Stack())
			}
		}()
		log.Printf("Mock OAuth2 Server 启动在 :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("监听失败: %v", err)
		}
	}()

	// 等待退出信号
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Mock OAuth2 Server 正在关闭...")

	// 优雅关闭
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("服务器关闭失败: %v", err)
		return
	}

	log.Println("服务器已退出")
}
