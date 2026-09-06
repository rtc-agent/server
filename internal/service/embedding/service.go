package embedding

import (
	"context"
	"fmt"
)

// Service Embedding 服务接口
type Service interface {
	// GenerateEmbedding 生成单条文本的 embedding
	GenerateEmbedding(ctx context.Context, text string) ([]float32, error)

	// GenerateEmbeddings 批量生成 embedding
	GenerateEmbeddings(ctx context.Context, texts []string) ([][]float32, error)

	// Dimension 返回向量维度
	Dimension() int
}

// Config Embedding 服务配置
type Config struct {
	// Enabled 是否启用
	Enabled bool

	// BaseURL API 端点（LM Studio 兼容 OpenAI API）
	BaseURL string

	// APIKey API 密钥（可选）
	APIKey string

	// Model 模型名称
	Model string

	// Dimension 向量维度
	Dimension int
}

// NewService 创建 Embedding 服务
// 当前只支持 OpenAI 兼容 API（LM Studio 也兼容）
func NewService(cfg Config) (Service, error) {
	if !cfg.Enabled {
		return &noopService{dimension: cfg.Dimension}, nil
	}

	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("embedding service: base_url is required")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("embedding service: model is required")
	}
	if cfg.Dimension <= 0 {
		cfg.Dimension = 1536 // 默认维度
	}

	return newOpenAIService(cfg)
}

// noopService 空实现（用于禁用 embedding 时）
type noopService struct {
	dimension int
}

func (s *noopService) GenerateEmbedding(ctx context.Context, text string) ([]float32, error) {
	return nil, fmt.Errorf("embedding service is disabled")
}

func (s *noopService) GenerateEmbeddings(ctx context.Context, texts []string) ([][]float32, error) {
	return nil, fmt.Errorf("embedding service is disabled")
}

func (s *noopService) Dimension() int {
	return s.dimension
}
